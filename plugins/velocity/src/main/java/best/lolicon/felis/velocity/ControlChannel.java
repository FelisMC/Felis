package best.lolicon.felis.velocity;

import best.lolicon.felis.link.Control;
import best.lolicon.felis.link.ControlFrame;
import best.lolicon.felis.link.FelisApiClient;
import best.lolicon.felis.link.LinkException;
import best.lolicon.felis.link.MenuStatus;
import best.lolicon.felis.link.ServerView;

import com.velocitypowered.api.event.Subscribe;
import com.velocitypowered.api.event.connection.DisconnectEvent;
import com.velocitypowered.api.event.connection.PluginMessageEvent;
import com.velocitypowered.api.proxy.Player;
import com.velocitypowered.api.proxy.ProxyServer;
import com.velocitypowered.api.proxy.ServerConnection;
import com.velocitypowered.api.proxy.messages.ChannelIdentifier;
import com.velocitypowered.api.proxy.messages.MinecraftChannelIdentifier;
import org.slf4j.Logger;

import java.util.ArrayList;
import java.util.Comparator;
import java.util.List;
import java.util.Map;
import java.util.UUID;
import java.util.concurrent.ConcurrentHashMap;

// Mark plugin messages handled synchronously before dispatching metered API work off-thread.
public final class ControlChannel implements WaitingRouter.MenuTransferListener {

    /** The namespaced channel both ends register; shared with the codec's name. */
    static final ChannelIdentifier CHANNEL = MinecraftChannelIdentifier.from(Control.CHANNEL);

    // How long one server's menu projection is reused across players.
    static final long STATUS_TTL_MILLIS = 2_000L;
    // Two full menu pages (45 tiles + the list each) in a burst, then 10 frames a second.
    private static final int FRAME_BURST = 96;
    private static final double FRAME_REFILL_PER_SECOND = 10.0;
    // Upper bound on tiles in one ListUpdate: a proxy-to-backend plugin message is capped at 32767 bytes, and 500 names with verdicts come to about 25 KB.
    static final int MAX_LISTED = 500;
    // The menu-access verdict for a server the player owns; those tiles lead the list.
    static final String MENU_OWNER = "owner";
    // A refused source is logged at most once per interval, so a hostile backend
    // cannot turn its own refusals into a log flood.
    private static final long REFUSAL_LOG_INTERVAL_MILLIS = 60_000L;

    private final ProxyServer proxy;
    private final Logger log;
    private final FelisApiClient api;
    private final WaitingRouter router;
    private final ServerRegistry registry;
    private final FelisVelocityPlugin plugin;
    private final ControlPolicy policy;
    private final String loginServer;
    private final String lobbyServer;
    private final FrameBudget budget =
            new FrameBudget(FRAME_BURST, FRAME_REFILL_PER_SECOND, System::currentTimeMillis);
    private final Map<String, CachedStatus> statusCache = new ConcurrentHashMap<>();
    private final Map<String, Long> lastRefusalLog = new ConcurrentHashMap<>();

    ControlChannel(ProxyServer proxy, Logger log, FelisApiClient api, WaitingRouter router,
                   ServerRegistry registry, FelisVelocityPlugin plugin,
                   String loginServer, String lobbyServer) {
        this.proxy = proxy;
        this.log = log;
        this.api = api;
        this.router = router;
        this.registry = registry;
        this.plugin = plugin;
        this.loginServer = loginServer;
        this.lobbyServer = lobbyServer;
        this.policy = new ControlPolicy(loginServer, lobbyServer, registry::isManaged);
    }

    /** register opens the channel and wires this instance as the waiting queue's menu-transfer listener. The caller still registers it as an event subscriber. */
    void register() {
        proxy.getChannelRegistrar().register(CHANNEL);
        router.setMenuTransferListener(this);
    }

    @Subscribe
    public void onPluginMessage(PluginMessageEvent event) {
        if (!CHANNEL.getId().equals(event.getIdentifier().getId())) {
            return; // not ours → leave Velocity's default handling alone
        }
        // A felis:control frame is never relayed to the other side: consume it before anything else.
        event.setResult(PluginMessageEvent.ForwardResult.handled());

        // Identity comes from the connection, never the frame. A frame from anything but a backend server (a client, say) is not a legitimate lobby action: drop it.
        if (!(event.getSource() instanceof ServerConnection)) {
            return;
        }
        ServerConnection source = (ServerConnection) event.getSource();
        Player player = source.getPlayer();
        String sourceName = source.getServerInfo().getName();

        ControlFrame frame;
        try {
            frame = Control.decode(event.getData());
        } catch (IllegalArgumentException e) {
            log.debug("Felis: dropping malformed felis:control frame from {}: {}", sourceName, e.getMessage());
            return;
        }

        ControlPolicy.Verdict verdict = policy.check(sourceName, frame);
        if (verdict != ControlPolicy.Verdict.ACCEPT) {
            refused(sourceName, frame, verdict);
            if (verdict == ControlPolicy.Verdict.BAD_SERVER && frame.server() != null
                    && ControlFrame.STATUS_QUERY.equals(frame.type())) {
                // The tile asked about a server that is gone (or never was): answer so
                // it stops saying "loading", without echoing a malformed name back.
                send(source, ControlFrame.error("not_found", "unknown server",
                        ControlPolicy.wellFormed(frame.server()) ? frame.server() : null));
            }
            return;
        }
        if (!budget.tryTake(player.getUniqueId())) {
            log.debug("Felis: felis:control budget exhausted for {}; dropping '{}'",
                    player.getUniqueId(), frame.type());
            return;
        }

        switch (frame.type()) {
            case ControlFrame.STATUS_QUERY:
                handleStatusQuery(source, frame.server());
                break;
            case ControlFrame.WAKE_REQUEST:
                handleWake(source, player, frame.server());
                break;
            case ControlFrame.CLAIM_REQUEST:
                handleClaim(source, player, frame.server());
                break;
            case ControlFrame.LIST_REQUEST:
                handleList(source, player);
                break;
            case ControlFrame.LOGIN_RELEASE:
                router.releaseFromLogin(player);
                break;
            default:
                // ControlPolicy admits only the types above.
                break;
        }
    }

    @Subscribe
    public void onDisconnect(DisconnectEvent event) {
        budget.forget(event.getPlayer().getUniqueId());
    }

    // Return server names without verdicts if the API or call pool is unavailable.
    private void handleList(ServerConnection source, Player player) {
        List<String> names = listed();
        UUID id = player.getUniqueId();
        boolean taken = plugin.async(() -> {
            Map<String, String> access;
            try {
                access = api.menuAccess(id);
            } catch (LinkException e) {
                log.debug("Felis: menu access for {} unavailable; listing names only: {}", id, e.getMessage());
                access = Map.of();
            }
            send(source, menuList(names, access));
        });
        if (!taken) {
            send(source, menuList(names, Map.of()));
        }
    }

    // Order owned servers first before applying the menu cap.
    static ControlFrame menuList(List<String> names, Map<String, String> access) {
        List<String> sorted = new ArrayList<>(names);
        sorted.sort(Comparator.comparing((String n) -> !MENU_OWNER.equals(access.get(n)))
                .thenComparing(Comparator.naturalOrder()));
        if (sorted.size() > MAX_LISTED) {
            sorted = sorted.subList(0, MAX_LISTED);
        }
        List<String> verdicts = new ArrayList<>(sorted.size());
        for (String n : sorted) {
            verdicts.add(access.getOrDefault(n, ""));
        }
        return ControlFrame.listUpdate(sorted, verdicts);
    }

    // listed is the lobby's tile set: every managed user server, by name. The system
    // servers are the lobby itself and the gate in front of it, so neither is a tile.
    private List<String> listed() {
        List<String> names = new ArrayList<>();
        for (ServerView v : registry.all()) {
            if (policy.isUserServer(v.name())) {
                names.add(v.name());
            }
        }
        return names;
    }

    private void refused(String sourceName, ControlFrame frame, ControlPolicy.Verdict verdict) {
        if (verdict == ControlPolicy.Verdict.BAD_SERVER) {
            log.debug("Felis: dropping felis:control '{}' from {}: not a managed user server",
                    frame.type(), sourceName);
            return;
        }
        long now = System.currentTimeMillis();
        Long last = lastRefusalLog.get(sourceName);
        if (last != null && now - last < REFUSAL_LOG_INTERVAL_MILLIS) {
            return;
        }
        lastRefusalLog.put(sourceName, now);
        log.warn("Felis: refused felis:control '{}' from backend {} ({}). Only {} may send menu frames "
                        + "and only {} may send LoginRelease; a user backend sending these is running "
                        + "a plugin that tries to act for the players on it.",
                frame.type(), sourceName, verdict, lobbyServer, loginServer);
    }

    // A StatusQuery refreshes one tile: read the menu projection and answer with a
    // StatusUpdate, or an Error if felis-api refuses (e.g. 404 unknown server).
    private void handleStatusQuery(ServerConnection source, String server) {
        CachedStatus cached = statusCache.get(server);
        if (cached != null && System.currentTimeMillis() - cached.atMillis <= STATUS_TTL_MILLIS) {
            send(source, cached.frame);
            return;
        }
        boolean taken = plugin.async(() -> {
            try {
                MenuStatus s = api.menuStatus(server);
                ControlFrame frame = ControlFrame.statusUpdate(
                        s.name(), s.phase(), s.ready(), s.playersOnline(), s.playersMax(), s.claimable());
                statusCache.put(server, new CachedStatus(frame, System.currentTimeMillis()));
                send(source, frame);
            } catch (LinkException e) {
                send(source, errorFrame(e, server));
            }
        });
        if (!taken) {
            send(source, busyFrame(server));
        }
    }

    // enqueueFromMenu performs wake admission and waiting off-thread and reports refusals itself.
    private void handleWake(ServerConnection source, Player player, String server) {
        router.enqueueFromMenu(player, server);
    }

    // Claim ownership and quota first; only successful claims proceed to policy-gated wake and queueing.
    private void handleClaim(ServerConnection source, Player player, String server) {
        UUID id = player.getUniqueId();
        boolean taken = plugin.async(() -> {
            try {
                api.claim(server, id);
            } catch (LinkException e) {
                send(source, errorFrame(e, server));
                return; // claim refused → do not wake a server the player doesn't own
            }
            statusCache.remove(server); // already claimed, so not claimable
            // Owned now → run the second rule. enqueueFromMenu spawns its own async
            // hop for the wake, which is fine from here.
            router.enqueueFromMenu(player, server);
        });
        if (!taken) {
            send(source, busyFrame(server));
        }
    }

    // TransferReady announces readiness; the proxy still performs the connection.
    @Override
    public void onReady(Player player, String serverName) {
        player.getCurrentServer().ifPresent(sc ->
                send(sc, ControlFrame.transferReady(player.getUsername(), serverName)));
    }

    private void send(ServerConnection connection, ControlFrame frame) {
        connection.sendPluginMessage(CHANNEL, Control.encode(frame));
    }

    // Hide transport details behind generic text; pass through structured API refusals.
    private static ControlFrame errorFrame(LinkException e, String server) {
        String code = e.errorCode() != null ? e.errorCode() : "error";
        String message = e.statusCode() == 0
                ? "Felis 暂时不可用，请稍后再试 / Felis is temporarily unavailable — please try again."
                : e.getMessage();
        return ControlFrame.error(code, message, server);
    }

    // busyFrame answers a frame whose felis-api call the bounded pool refused.
    private static ControlFrame busyFrame(String server) {
        return ControlFrame.error("busy",
                "Felis 现在很忙，请稍后再试 / Felis is busy right now — please try again in a moment.", server);
    }

    private static final class CachedStatus {
        final ControlFrame frame;
        final long atMillis;

        CachedStatus(ControlFrame frame, long atMillis) {
            this.frame = frame;
            this.atMillis = atMillis;
        }
    }
}
