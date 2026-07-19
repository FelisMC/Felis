package best.lolicon.felis.velocity;

import best.lolicon.felis.link.Control;
import best.lolicon.felis.link.ControlFrame;
import best.lolicon.felis.link.FelisApiClient;
import best.lolicon.felis.link.LinkException;
import best.lolicon.felis.link.MenuStatus;

import com.velocitypowered.api.event.Subscribe;
import com.velocitypowered.api.event.connection.PluginMessageEvent;
import com.velocitypowered.api.proxy.Player;
import com.velocitypowered.api.proxy.ProxyServer;
import com.velocitypowered.api.proxy.ServerConnection;
import com.velocitypowered.api.proxy.messages.ChannelIdentifier;
import com.velocitypowered.api.proxy.messages.MinecraftChannelIdentifier;
import org.slf4j.Logger;

import java.util.UUID;

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
 * never from the frame's {@code player} field, so a compromised backend cannot drive
 * an action as another player. A frame whose source is not a backend server (e.g. a
 * client) is consumed and dropped. The frame's {@code server} field is data, not
 * identity: it names which backend the player asked for, and the autostartPolicy /
 * ownership gates server-side decide whether this UUID may act on it.
 *
 * <p><b>Threading.</b> {@code felis:control} frames arrive on a Velocity event
 * thread, but every felis-api call below blocks on HTTP. So each handler does the
 * non-blocking work synchronously — decode, derive identity, and
 * {@code setResult(handled())} to stop the frame being forwarded — then hands the
 * blocking call to {@link FelisVelocityPlugin#async}, sending any downstream frame
 * from inside that callback. {@code setResult} must run before the handler returns;
 * it cannot be set from the async hop.
 *
 * <p>The three upstream frames map onto the menu's buttons (spec §12): a
 * {@code StatusQuery} refreshes a tile ({@code StatusUpdate} back); a
 * {@code WakeRequest} (owned server) wakes and parks; a {@code ClaimRequest}
 * (ownerless server) runs the two-rule split — claim asserts ownership/quota, then
 * the wake applies the autostartPolicy gate — and parks on success. Refusals come
 * back as {@code Error}; readiness as {@code TransferReady} just before the proxy
 * Connects the player (via {@link WaitingRouter.MenuTransferListener}).
 */
public final class ControlChannel implements WaitingRouter.MenuTransferListener {

    /** The namespaced channel both ends register; shared with the codec's name. */
    static final ChannelIdentifier CHANNEL = MinecraftChannelIdentifier.from(Control.CHANNEL);

    private final ProxyServer proxy;
    private final Logger log;
    private final FelisApiClient api;
    private final WaitingRouter router;
    private final FelisVelocityPlugin plugin;

    ControlChannel(ProxyServer proxy, Logger log, FelisApiClient api,
                   WaitingRouter router, FelisVelocityPlugin plugin) {
        this.proxy = proxy;
        this.log = log;
        this.api = api;
        this.router = router;
        this.plugin = plugin;
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

        ControlFrame frame;
        try {
            frame = Control.decode(event.getData());
        } catch (IllegalArgumentException e) {
            log.debug("Felis: dropping malformed felis:control frame from {}: {}",
                    source.getServerInfo().getName(), e.getMessage());
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
            default:
                // Downstream-only types (StatusUpdate/TransferReady/Error) are not
                // actionable arriving upstream; a well-behaved lobby never sends them.
                log.debug("Felis: ignoring non-actionable felis:control frame '{}' from {}",
                        frame.type(), player.getUsername());
        }
    }

    // A StatusQuery refreshes one tile: read the menu projection and answer with a
    // StatusUpdate, or an Error if felis-api refuses (e.g. 404 unknown server).
    private void handleStatusQuery(ServerConnection source, String server) {
        if (isBlank(server)) {
            return; // nothing to look up
        }
        plugin.async(() -> {
            try {
                MenuStatus s = api.menuStatus(server);
                send(source, ControlFrame.statusUpdate(
                        s.name(), s.phase(), s.ready(), s.playersOnline(), s.playersMax(), s.claimable()));
            } catch (LinkException e) {
                send(source, errorFrame(e, server));
            }
        });
    }

    // A WakeRequest is the menu's Join/Wake button on a server the player owns: wake
    // it and park them in the shared queue. enqueueFromMenu does the HTTP off-thread
    // and reports its own refusals to the player; nothing to await here.
    private void handleWake(ServerConnection source, Player player, String server) {
        if (isBlank(server)) {
            send(source, ControlFrame.error("bad_request", "wake without a server", null));
            return;
        }
        router.enqueueFromMenu(player, server);
    }

    // A ClaimRequest is the menu's Claim & Start on an ownerless server: the two-rule
    // split. Claim first (ownership + quota); only on success wake-and-park (the
    // autostartPolicy gate). A claim refusal answers with Error and never wakes.
    private void handleClaim(ServerConnection source, Player player, String server) {
        if (isBlank(server)) {
            send(source, ControlFrame.error("bad_request", "claim without a server", null));
            return;
        }
        UUID id = player.getUniqueId();
        plugin.async(() -> {
            try {
                api.claim(server, id);
            } catch (LinkException e) {
                send(source, errorFrame(e, server));
                return; // claim refused → do not wake a server the player doesn't own
            }
            // Owned now → run the second rule. enqueueFromMenu spawns its own async
            // hop for the wake, which is fine from here.
            router.enqueueFromMenu(player, server);
        });
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

    private static boolean isBlank(String s) {
        return s == null || s.isEmpty();
    }
}
