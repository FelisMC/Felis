package best.lolicon.felis.velocity;

import best.lolicon.felis.link.FelisApiClient;
import best.lolicon.felis.link.LinkException;
import best.lolicon.felis.link.ServerView;

import com.velocitypowered.api.event.Subscribe;
import com.velocitypowered.api.event.player.PlayerChooseInitialServerEvent;
import com.velocitypowered.api.event.player.ServerConnectedEvent;
import com.velocitypowered.api.proxy.Player;
import com.velocitypowered.api.proxy.ProxyServer;
import com.velocitypowered.api.proxy.server.RegisteredServer;
import net.kyori.adventure.text.Component;
import net.kyori.adventure.text.format.NamedTextColor;
import org.slf4j.Logger;

import java.net.InetSocketAddress;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.Locale;
import java.util.Map;
import java.util.Optional;
import java.util.UUID;
import java.util.concurrent.ConcurrentHashMap;

/**
 * WaitingRouter implements the §11 domain-autostart routing loop and its waiting
 * queue. It resolves the virtual host a player connected with to a felis server
 * and decides what happens next:
 *
 * <pre>
 *   host has no felis subdomain        → leave Velocity's default routing alone
 *   server ready + registered          → set it as the initial server (straight in)
 *   server not ready, lobby configured → park in lobby, wake it, enqueue a transfer
 *   server not ready, no lobby          → refuse cleanly ("reconnect shortly"), wake
 * </pre>
 *
 * <p>The queue is drained by {@link #tick()}, scheduled by the plugin on the async
 * pool. Each tick polls felis-api once per distinct waited-on server and, when one
 * reports ready, transfers everyone waiting on it. A waiter drops out when it times
 * out, when the player leaves the proxy, or on a successful transfer.
 *
 * <p>The wake is gated server-side by autostartPolicy keyed on the player's
 * online-mode UUID: a 403 means this player may not start the server (we tell them
 * and stop), a 429 means a wake is already in flight (we keep waiting). Real joins
 * to a felis backend are reported back so the reaper sees activity and the player
 * is auto-added to the allowlist.
 */
public final class WaitingRouter {
    private static final long WAIT_TIMEOUT_MILLIS = 120_000L;

    private final ProxyServer proxy;
    private final Logger log;
    private final FelisApiClient api;
    private final ServerRegistry registry;
    private final FelisVelocityPlugin plugin;
    private final String lobbyServer; // may be null → no lobby

    private final Map<UUID, Waiter> waiting = new ConcurrentHashMap<>();

    // Notified just before a menu-originated waiter is transferred, so the lobby's
    // felis:control face can tell the player's GUI the backend is ready. Null until
    // the ControlChannel is wired in at proxy init; set once, read on the tick pool.
    private volatile MenuTransferListener menuListener;

    WaitingRouter(ProxyServer proxy, Logger log, FelisApiClient api, ServerRegistry registry,
                  FelisVelocityPlugin plugin, String lobbyServer) {
        this.proxy = proxy;
        this.log = log;
        this.api = api;
        this.registry = registry;
        this.plugin = plugin;
        this.lobbyServer = lobbyServer;
    }

    /**
     * setMenuTransferListener wires the felis:control face so a menu-driven wait can
     * notify the lobby when its backend is ready. Called once at proxy init.
     */
    void setMenuTransferListener(MenuTransferListener listener) {
        this.menuListener = listener;
    }

    /**
     * enqueueFromMenu parks a player who is already on the proxy (sitting in the
     * lobby) on a server they asked for through the felis-paper {@code /menu}, then
     * wakes it and lets {@link #tick()} transfer them when ready — the same shared
     * waiting queue used by host-based autostart routing (spec §12, §27 scenario 10).
     * It differs from initial-server routing only in origin: the player drove it from
     * a GUI button rather than a connecting virtual host, so the waiter is flagged to
     * fire {@link MenuTransferListener} on transfer.
     */
    void enqueueFromMenu(Player player, String serverName) {
        wakeAndWait(player, serverName, true);
    }

    @Subscribe
    public void onChooseInitialServer(PlayerChooseInitialServerEvent event) {
        Player player = event.getPlayer();
        Optional<String> host = virtualHost(player);
        if (host.isEmpty()) {
            return; // direct connect / no SRV host → leave default routing
        }
        Optional<ServerView> targetOpt = registry.resolveByHost(host.get());
        if (targetOpt.isEmpty()) {
            return; // host is not a felis subdomain → leave default routing
        }
        ServerView target = targetOpt.get();
        Optional<RegisteredServer> backend = registry.registered(target.name());
        if (target.ready() && backend.isPresent()) {
            event.setInitialServer(backend.get()); // ready → straight in
            return;
        }

        Optional<RegisteredServer> lobby = lobby();
        if (lobby.isEmpty()) {
            // Nowhere to hold the player while the backend wakes: refuse cleanly so
            // they reconnect onto a ready server, rather than dropping them onto a
            // backend that is still starting. Still fire the wake so the reconnect
            // lands faster.
            player.disconnect(Component.text(
                    "« " + target.name() + " » is starting up — please reconnect in a moment.",
                    NamedTextColor.YELLOW));
            fireWake(player.getUniqueId(), target.name());
            return;
        }
        event.setInitialServer(lobby.get()); // park in lobby
        wakeAndWait(player, target.name(), false);
    }

    @Subscribe
    public void onServerConnected(ServerConnectedEvent event) {
        String name = event.getServer().getServerInfo().getName();
        if (!registry.isManaged(name)) {
            return; // lobby / static server → not a felis backend, nothing to report
        }
        UUID id = event.getPlayer().getUniqueId();
        plugin.async(() -> {
            try {
                api.reportJoin(name, id);
            } catch (LinkException e) {
                log.debug("Felis: join-event {} failed (status={}): {}", name, e.statusCode(), e.getMessage());
            }
        });
    }

    /** tick drains the waiting queue; the plugin schedules it on the async pool. */
    void tick() {
        if (waiting.isEmpty()) {
            return;
        }
        long now = System.currentTimeMillis();
        Map<String, Boolean> readyCache = new HashMap<>(); // one status poll per distinct server
        for (Map.Entry<UUID, Waiter> e : new ArrayList<>(waiting.entrySet())) {
            UUID id = e.getKey();
            Waiter w = e.getValue();
            Optional<Player> po = proxy.getPlayer(id);
            if (po.isEmpty()) {
                waiting.remove(id); // player left the proxy
                continue;
            }
            Player player = po.get();
            if (now > w.deadlineMillis) {
                waiting.remove(id);
                player.sendMessage(Component.text(
                        "« " + w.serverName + " » is taking longer than expected to start. "
                        + "You can keep waiting in the lobby or try again later.", NamedTextColor.YELLOW));
                continue;
            }
            Boolean ready = readyCache.get(w.serverName);
            if (ready == null) {
                try {
                    ready = api.serverStatus(w.serverName).ready();
                } catch (LinkException ex) {
                    ready = Boolean.FALSE; // transient → keep waiting until the deadline
                }
                readyCache.put(w.serverName, ready);
            }
            if (!ready) {
                continue;
            }
            Optional<RegisteredServer> backend = registry.registered(w.serverName);
            if (backend.isEmpty()) {
                continue; // ready but not yet registered → next tick
            }
            waiting.remove(id);
            player.sendMessage(Component.text(
                    "« " + w.serverName + " » is ready — moving you in…", NamedTextColor.GREEN));
            // Tell a menu-driven lobby its tile is live before we pull the player off
            // it; the proxy still performs the actual Connect just below.
            MenuTransferListener listener = menuListener;
            if (w.fromMenu && listener != null) {
                listener.onReady(player, w.serverName);
            }
            transfer(player, w.serverName, backend.get());
        }
    }

    private void wakeAndWait(Player player, String serverName, boolean fromMenu) {
        UUID id = player.getUniqueId();
        plugin.async(() -> {
            try {
                api.wake(serverName, id);
            } catch (LinkException e) {
                switch (e.statusCode()) {
                    case 403:
                        player.sendMessage(Component.text(
                                "You're not allowed to start « " + serverName + " ».", NamedTextColor.RED));
                        return; // policy gate refused → do not enqueue
                    case 429:
                        break; // a wake is already in flight → fall through to waiting
                    default:
                        log.warn("Felis: wake {} failed (status={}): {}", serverName, e.statusCode(), e.getMessage());
                        player.sendMessage(Component.text(
                                "Couldn't start « " + serverName + " » right now. Try again shortly.",
                                NamedTextColor.RED));
                        return;
                }
            }
            player.sendMessage(Component.text(
                    "Starting « " + serverName + " » — you'll be moved in automatically.",
                    NamedTextColor.GRAY));
            waiting.put(id, new Waiter(serverName, System.currentTimeMillis() + WAIT_TIMEOUT_MILLIS, fromMenu));
        });
    }

    private void fireWake(UUID id, String serverName) {
        plugin.async(() -> {
            try {
                api.wake(serverName, id);
            } catch (LinkException e) {
                if (e.statusCode() != 429 && e.statusCode() != 403) {
                    log.warn("Felis: wake {} failed (status={}): {}", serverName, e.statusCode(), e.getMessage());
                }
            }
        });
    }

    private void transfer(Player player, String serverName, RegisteredServer backend) {
        player.createConnectionRequest(backend).connect().whenComplete((result, err) -> {
            if (err != null || (result != null && !result.isSuccessful())) {
                player.sendMessage(Component.text(
                        "Couldn't connect you to « " + serverName + " ». Please try again.",
                        NamedTextColor.RED));
            }
        });
    }

    private Optional<RegisteredServer> lobby() {
        return lobbyServer == null ? Optional.empty() : proxy.getServer(lobbyServer);
    }

    private static Optional<String> virtualHost(Player player) {
        return player.getVirtualHost()
                .map(InetSocketAddress::getHostString)
                .map(s -> s.toLowerCase(Locale.ROOT));
    }

    private static final class Waiter {
        final String serverName;
        final long deadlineMillis;
        final boolean fromMenu; // true → notify the felis:control face on transfer

        Waiter(String serverName, long deadlineMillis, boolean fromMenu) {
            this.serverName = serverName;
            this.deadlineMillis = deadlineMillis;
            this.fromMenu = fromMenu;
        }
    }

    /**
     * MenuTransferListener bridges the shared waiting queue to the felis:control face
     * without {@link WaitingRouter} depending on the wire codec: it is told a
     * menu-originated player's backend is ready, and the implementation owns encoding
     * and sending the {@code TransferReady} frame. The proxy still performs the
     * Connect itself ({@link #transfer}); this is only the lobby-UI notification.
     */
    interface MenuTransferListener {
        void onReady(Player player, String serverName);
    }
}
