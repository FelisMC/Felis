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

/**
 * ControlChannel is the proxy end of the {@code felis:control} plugin-message channel
 * (spec §12): the bridge between the felis-paper lobby's {@code /menu} GUI and the
 * routing core. The lobby is a pure UI face — it holds no felis-api token and keeps
 * no queue — so every menu action arrives here as a {@link ControlFrame}, this class
 * drives the felis-api internal endpoints and the shared waiting queue, and answers
 * downstream with another frame. It is the §27 scenario-10 path: {@code /menu →
 * plugin msg → velocity → api → 共用等待队列 → ready 后 Connect}.
 *
 * <p><b>Anti-spoof (spec §14).</b> The acting identity is taken from the
 * {@link ServerConnection} the message arrived on — {@code source.getPlayer()} — and
 * never from the frame's {@code player} field. That alone does not make a frame
 * trustworthy: every user backend runs plugins its owner chose, and the player on a
 * connection is whoever happens to be standing on that server. So the connection's
 * <em>server</em> decides what may be sent at all ({@link ControlPolicy}): the lobby
 * gets the menu frames, the login gate gets {@code LoginRelease}, and every other
 * backend is dropped. A frame from a client is dropped too. The {@code server} field
 * must name a managed user server; the autostartPolicy / ownership gates
 * server-side then decide whether this UUID may act on it.
 *
 * <p><b>Load.</b> Each accepted frame is at most one blocking felis-api call, so
 * frames are metered per player ({@link FrameBudget}) and menu projections are
 * shared across players for {@link #STATUS_TTL_MILLIS}: a lobby full of players
 * opening the menu at once reads each server's status once, not once per player.
 * The one per-player read is a menu open's {@code ListRequest}: a single call that
 * says what the player may do with every tile.
 *
 * <p><b>Threading.</b> {@code felis:control} frames arrive on a Velocity event
 * thread, but every felis-api call below blocks on HTTP. So each handler does the
 * non-blocking work synchronously — decode, derive identity, and
 * {@code setResult(handled())} to stop the frame being forwarded — then hands the
 * blocking call to {@link FelisVelocityPlugin#async}, sending any downstream frame
 * from inside that callback. {@code setResult} must run before the handler returns;
 * it cannot be set from the async hop.
 *
 * <p>The lobby's upstream frames map onto the menu (spec §12): a {@code ListRequest}
 * asks which tiles to draw ({@code ListUpdate} back: the registry's names, the
 * player's own first, each with what felis-api says the player may do with it); a
 * {@code StatusQuery} refreshes a tile ({@code StatusUpdate} back); a
 * {@code WakeRequest} (owned server) wakes and parks; a {@code ClaimRequest}
 * (ownerless server) runs the two-rule split — claim asserts ownership/quota, then
 * the wake applies the autostartPolicy gate — and parks on success. Refusals come
 * back as {@code Error}; readiness as {@code TransferReady} just before the proxy
 * Connects the player (via {@link WaitingRouter.MenuTransferListener}).
 *
 * <p>The login gate's one frame, {@code LoginRelease}, asks the proxy to move the
 * player to the lobby. It replaces the BungeeCord {@code Connect} the gate used to
 * send: {@code bungeecord:main} is handled inside Velocity before any plugin event,
 * so it cannot be restricted to the gate and is switched off in velocity.toml
 * instead. The release itself is still authorized by
 * {@link WaitingRouter#onServerPreConnect}.
 */
public final class ControlChannel implements WaitingRouter.MenuTransferListener {

    /** The namespaced channel both ends register; shared with the codec's name. */
    static final ChannelIdentifier CHANNEL = MinecraftChannelIdentifier.from(Control.CHANNEL);

    // How long one server's menu projection is reused across players.
    static final long STATUS_TTL_MILLIS = 2_000L;
    // Two full menu pages (45 tiles + the list each) in a burst, then 10 frames a second.
    private static final int FRAME_BURST = 96;
    private static final double FRAME_REFILL_PER_SECOND = 10.0;
    // Upper bound on tiles in one ListUpdate. A proxy→backend plugin message is capped
    // at 32767 bytes; 500 names of at most 32 chars plus 500 verdicts of at most 12
    // (each quoted, comma-separated) come to about 25 KB.
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

    /**
     * register opens the channel and wires this instance as the waiting queue's
     * menu-transfer listener. The caller still registers it as an event subscriber.
     */
    void register() {
        proxy.getChannelRegistrar().register(CHANNEL);
        router.setMenuTransferListener(this);
    }

    @Subscribe
    public void onPluginMessage(PluginMessageEvent event) {
        if (!CHANNEL.getId().equals(event.getIdentifier().getId())) {
            return; // not ours → leave Velocity's default handling alone
        }
        // We own this channel end to end: a felis:control frame is never relayed to
        // the other side, whatever its source. Consume it before doing anything else.
        event.setResult(PluginMessageEvent.ForwardResult.handled());

        // Identity comes from the connection, never the frame (spec §14). A frame from
        // anything but a backend server (e.g. a client) is not a legitimate lobby
        // action — drop it.
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

    // A ListRequest draws the menu: the names come from the registry, and what this
    // player may do with each comes from felis-api in one call. When felis-api cannot
    // answer (or the pool is full) the names still go out, and the lobby draws its
    // tiles without verdicts, as before they existed.
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

    /**
     * menuList orders the tiles and pairs each with its verdict: the player's own
     * servers first, then everything else, each group by name. The cap applies after
     * the ordering, so a player's own servers are never the ones cut.
     */
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

    // A WakeRequest is the menu's Join or Start button: wake
    // it and park them in the shared queue. enqueueFromMenu does the HTTP off-thread
    // and reports its own refusals to the player; nothing to await here.
    private void handleWake(ServerConnection source, Player player, String server) {
        router.enqueueFromMenu(player, server);
    }

    // A ClaimRequest is the menu's Claim & Start on an ownerless server: the two-rule
    // split. Claim first (ownership + quota); only on success wake-and-park (the
    // autostartPolicy gate). A claim refusal answers with Error and never wakes.
    private void handleClaim(ServerConnection source, Player player, String server) {
        UUID id = player.getUniqueId();
        boolean taken = plugin.async(() -> {
            try {
                api.claim(server, id);
            } catch (LinkException e) {
                send(source, errorFrame(e, server));
                return; // claim refused → do not wake a server the player doesn't own
            }
            statusCache.remove(server); // it is no longer claimable
            // Owned now → run the second rule. enqueueFromMenu spawns its own async
            // hop for the wake, which is fine from here.
            router.enqueueFromMenu(player, server);
        });
        if (!taken) {
            send(source, busyFrame(server));
        }
    }

    /**
     * onReady fires when a menu-parked player's backend goes ready, just before the
     * proxy Connects them. The lobby uses {@code TransferReady} to react (close the
     * menu / show "joining"); the actual move is the proxy's Connect, not this frame.
     */
    @Override
    public void onReady(Player player, String serverName) {
        player.getCurrentServer().ifPresent(sc ->
                send(sc, ControlFrame.transferReady(player.getUsername(), serverName)));
    }

    private void send(ServerConnection connection, ControlFrame frame) {
        connection.sendPluginMessage(CHANNEL, Control.encode(frame));
    }

    // errorFrame turns a LinkException into a downstream Error. felis-api's structured
    // errors carry user-safe text (the same {code,message} the external API returns),
    // but a transport failure (statusCode 0) carries internal IO detail — host names,
    // refused ports — that must not reach a player's screen, so it is generalized.
    // The lobby localizes known codes itself; this fallback message may reach the
    // screen raw, so it carries both languages in one line (the no-locale pattern).
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
