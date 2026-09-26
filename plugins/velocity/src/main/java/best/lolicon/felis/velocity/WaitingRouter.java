package best.lolicon.felis.velocity;

import best.lolicon.felis.link.FelisApiClient;
import best.lolicon.felis.link.LinkException;
import best.lolicon.felis.link.ServerView;

import com.velocitypowered.api.event.EventTask;
import com.velocitypowered.api.event.Subscribe;
import com.velocitypowered.api.event.connection.DisconnectEvent;
import com.velocitypowered.api.event.player.PlayerChooseInitialServerEvent;
import com.velocitypowered.api.event.player.ServerConnectedEvent;
import com.velocitypowered.api.event.player.ServerPreConnectEvent;
import com.velocitypowered.api.proxy.ConnectionRequestBuilder;
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
import java.util.concurrent.atomic.AtomicBoolean;

/**
 * WaitingRouter implements the §11 domain-autostart routing loop and its waiting
 * queue. It resolves the virtual host a player connected with to a felis server
 * and remembers the requested backend while the player passes the login gate:
 *
 * <pre>
 *   fresh connection                  → login (always; never a user backend)
 *   login says linked + target ready  → requested backend
 *   login says linked + target asleep → post-auth lobby, wake, queued transfer
 *   login requests any other target   → deny (fail closed)
 * </pre>
 *
 * <p>The queue is drained by {@link #tick()}, scheduled by the plugin on the async
 * pool. Each tick polls felis-api once per distinct waited-on server and, when one
 * reports ready, transfers everyone waiting on it. A waiter drops out when it times
 * out, when the player leaves the proxy, or on a successful transfer.
 *
 * <p>Every transition out of login is checked against felis-api's link status, and
 * command/menu queue entries are checked the same way. The wake is then gated
 * server-side by autostartPolicy keyed on the player's online-mode UUID: a 403 means
 * this player may not start the server (we tell them and stop), a 429 means a wake is
 * already in flight (we keep waiting), and a 503 means the cluster is at capacity
 * (we tell them to try later — nothing is coming up, so we do not enqueue). Real
 * user-backend joins are reported back so the reaper sees activity and the player is
 * auto-added to the allowlist.
 *
 * <p>Link checks go through {@link LinkGate}, which rides out a short felis-api
 * outage on a recent positive answer for the same UUID and otherwise fails closed.
 */
public final class WaitingRouter {
    private static final long WAIT_TIMEOUT_MILLIS = 120_000L;
    // How long a positive link answer can stand in for felis-api while it is down.
    private static final long LINK_GRACE_MILLIS = 10 * 60_000L;
    // The login gate re-sends its release with backoff (and during a felis-api outage
    // on every retry), so a denial line is shown at most once per this interval.
    private static final long GATE_NOTICE_INTERVAL_MILLIS = 15_000L;

    private final ProxyServer proxy;
    private final Logger log;
    private final FelisApiClient api;
    private final ServerRegistry registry;
    private final FelisVelocityPlugin plugin;
    private final String loginServer;
    private final String lobbyServer;

    private final LinkGate links;
    private final Map<UUID, Waiter> waiting = new ConcurrentHashMap<>();
    private final Map<UUID, String> pendingTargets = new ConcurrentHashMap<>();
    private final Map<UUID, Long> lastGateNotice = new ConcurrentHashMap<>();
    // tick is scheduled at a fixed rate and makes blocking calls; when felis-api is
    // slow a run can outlast the interval, and overlapping runs would multiply the
    // load on the thing that is already slow.
    private final AtomicBoolean ticking = new AtomicBoolean(false);

    // Notified just before a menu-originated waiter is transferred, so the lobby's
    // felis:control face can tell the player's GUI the backend is ready. Null until
    // the ControlChannel is wired in at proxy init; set once, read on the tick pool.
    private volatile MenuTransferListener menuListener;

    WaitingRouter(ProxyServer proxy, Logger log, FelisApiClient api, ServerRegistry registry,
                  FelisVelocityPlugin plugin, String loginServer, String lobbyServer) {
        this.proxy = proxy;
        this.log = log;
        this.api = api;
        this.registry = registry;
        this.plugin = plugin;
        this.loginServer = loginServer;
        this.lobbyServer = lobbyServer;
        this.links = new LinkGate(api::linkStatus, LINK_GRACE_MILLIS, System::currentTimeMillis);
    }

    /** pruneLinks bounds the LinkGate's fallback records; called on the refresh loop. */
    void pruneLinks() {
        links.prune();
    }

    /**
     * releaseFromLogin is the proxy end of the login gate's {@code LoginRelease}: ask
     * Velocity to move the player to the lobby. The move is authorized where every
     * login exit is, in {@link #onServerPreConnect}, so a gate that sends this early
     * gets a denial and nothing else. ControlChannel has already checked the frame
     * came from the login server, so the player is standing on it.
     */
    void releaseFromLogin(Player player) {
        Optional<RegisteredServer> lobby = proxy.getServer(lobbyServer);
        if (lobby.isEmpty()) {
            log.warn("Felis: login release for {} but the lobby '{}' is not registered yet",
                    player.getUniqueId(), lobbyServer);
            return; // the gate retries; the next refresh registers the lobby
        }
        player.createConnectionRequest(lobby.get()).connect().whenComplete((result, err) -> {
            if (err != null) {
                log.warn("Felis: login release for {} failed: {}", player.getUniqueId(), err.toString());
                return;
            }
            if (result != null && !result.isSuccessful()) {
                releaseRefused(player, result);
            }
        });
    }

    /**
     * releaseRefused handles a login release that did not land. When the pre-connect
     * check had redirected it to the player's requested server and that server turned
     * them away — banned, not whitelisted, full, the wrong mods — the remembered target
     * would send every retry of the gate into the same refusal, silently, until the gate
     * gave up two minutes later with "couldn't enter the lobby". Forget the target, show
     * the server's own reason, and release the player to the lobby at once.
     */
    private void releaseRefused(Player player, ConnectionRequestBuilder.Result result) {
        UUID id = player.getUniqueId();
        RegisteredServer attempted = result.getAttemptedConnection();
        String pending = pendingTargets.get(id);
        if (attempted == null || pending == null || !serverNamed(attempted, pending)
                || !pendingTargets.remove(id, pending)) {
            log.warn("Felis: login release for {} did not land: {}", id, result.getStatus());
            return; // a failed move to the lobby itself: the gate retries
        }
        boolean zh = FelisVelocityPlugin.zh(player);
        Component line = Component.text(
                zh ? "「" + pending + "」拒绝了你的连接，已把你送到大厅。"
                   : "« " + pending + " » turned you away, so you're in the lobby instead.",
                NamedTextColor.RED);
        Optional<Component> reason = result.getReasonComponent();
        if (reason.isPresent()) {
            line = line.append(Component.text(zh ? " 原因：" : " Reason: ", NamedTextColor.RED))
                    .append(reason.get());
        }
        player.sendMessage(line);
        log.info("Felis: {} refused {} on the login release ({})", pending, id, result.getStatus());
        releaseFromLogin(player);
    }

    // linked runs the link check through the gate and notes when the answer came from
    // the outage fallback rather than felis-api.
    private boolean linked(UUID id) throws LinkException {
        LinkGate.Result r = links.check(id);
        if (r.degraded) {
            log.warn("Felis: felis-api unreachable; admitting {} on a link confirmation from the last {} min",
                    id, LINK_GRACE_MILLIS / 60_000L);
        }
        return r.linked;
    }

    /**
     * setMenuTransferListener wires the felis:control face so a menu-driven wait can
     * notify the lobby when its backend is ready. Called once at proxy init.
     */
    void setMenuTransferListener(MenuTransferListener listener) {
        this.menuListener = listener;
    }

    /**
     * enqueueFromMenu moves a player who is already on the proxy (sitting in the
     * lobby) to a server they asked for through the felis-paper {@code /menu}: straight
     * in when it is running, otherwise woken and transferred by {@link #tick()} once
     * ready — the same shared waiting queue used by host-based autostart routing (spec
     * §12, §27 scenario 10). It differs from initial-server routing only in origin: the
     * player drove it from a GUI button rather than a connecting virtual host, so the
     * move fires {@link MenuTransferListener}.
     */
    void enqueueFromMenu(Player player, String serverName) {
        authorizeAndWait(player, serverName, true);
    }

    /**
     * enqueueFromCommand is {@link #enqueueFromMenu} for {@code /felis go <server>} typed
     * in chat, differing only in that it is NOT flagged {@code fromMenu}: a command has no
     * felis-paper GUI tile to notify, so no {@code TransferReady} frame is emitted. A wake
     * stays autostartPolicy-gated on the verified UUID exactly as for the other origins,
     * so this adds a new entry point, not a new authority.
     */
    void enqueueFromCommand(Player player, String serverName) {
        authorizeAndWait(player, serverName, false);
    }

    /**
     * enqueueFromInvite is {@link #enqueueFromCommand} for an accepted invite. An invite can
     * only name the server its sender is standing on, so the target is running by
     * construction and the invitee is joined straight onto it.
     *
     * <p>It does leave a mark, though, and one that outlives the invite: landing there fires
     * {@link #onServerConnected}, whose join-event appends the player to the server's
     * allowlist. On an autostartPolicy=allowlist server that row is the wake permission, so
     * an accepted invite ends in the invitee being able to start the server later. That is
     * the same row they would have earned by walking in unaided — the invite shortened the
     * walk, it did not widen the door — but it is a consequence the INVITER is warned about
     * up front (FelisVelocityPlugin#accessNotice), because they are the one causing it.
     */
    void enqueueFromInvite(Player player, String serverName) {
        authorizeAndWait(player, serverName, false);
    }

    @Subscribe
    public void onChooseInitialServer(PlayerChooseInitialServerEvent event) {
        Player player = event.getPlayer();
        UUID id = player.getUniqueId();
        pendingTargets.remove(id); // a reconnect must never inherit an earlier host
        Optional<String> host = virtualHost(player);
        if (host.isEmpty()) {
            return; // velocity.toml's only fallback is login
        }
        Optional<ServerView> targetOpt = registry.resolveByHost(host.get());
        if (targetOpt.isEmpty()) {
            return; // unknown host also falls through to login
        }
        ServerView target = targetOpt.get();
        Optional<RegisteredServer> login = login();
        if (login.isEmpty()) {
            event.setInitialServer(null);
            player.disconnect(Component.text(
                    FelisVelocityPlugin.zh(player)
                            ? "Felis 登录网关不可用，请稍后重连。"
                            : "The Felis login gate is unavailable. Please reconnect shortly.",
                    NamedTextColor.RED));
            return;
        }
        // login.<root-domain> is a valid system hostname, but it is the gate rather
        // than a post-auth destination. Remembering it would redirect the successful
        // lobby release straight back into login and loop forever.
        if (!target.name().equalsIgnoreCase(loginServer)) {
            pendingTargets.put(id, target.name());
        }
        event.setInitialServer(login.get());
    }

    /**
     * The login backend requests the configured lobby only after its own link poll
     * succeeds. Re-check that state on the trusted proxy boundary, then either route
     * the remembered virtual-host target or admit the player to the post-auth lobby.
     */
    @Subscribe
    public EventTask onServerPreConnect(ServerPreConnectEvent event) {
        RegisteredServer previous = event.getPreviousServer();
        if (previous == null || !serverNamed(previous, loginServer)) {
            return null;
        }

        Player player = event.getPlayer();
        event.setResult(ServerPreConnectEvent.ServerResult.denied());
        if (!serverNamed(event.getOriginalServer(), lobbyServer)) {
            player.sendMessage(Component.text(
                    FelisVelocityPlugin.zh(player)
                            ? "登录网关只能把玩家放行到大厅。"
                            : "The login gate may only release players to the lobby.",
                    NamedTextColor.RED));
            log.warn("Felis: denied login-gate transfer for {} to {}",
                    player.getUniqueId(), event.getOriginalServer().getServerInfo().getName());
            return null;
        }

        return EventTask.async(() -> authorizeLoginRelease(event));
    }

    private void authorizeLoginRelease(ServerPreConnectEvent event) {
        Player player = event.getPlayer();
        UUID id = player.getUniqueId();
        boolean zh = FelisVelocityPlugin.zh(player);
        try {
            if (!linked(id)) {
                gateNotice(player, Component.text(
                        zh ? "请先完成登录，再离开登录区。"
                           : "Finish signing in before leaving the login area.", NamedTextColor.YELLOW));
                return;
            }
        } catch (LinkException e) {
            log.warn("Felis: could not verify login release for {} (status={}): {}",
                    id, e.statusCode(), e.getMessage());
            gateNotice(player, Component.text(
                    zh ? "登录验证暂时不可用，请稍候重试。"
                       : "Login verification is temporarily unavailable. Please wait and try again.",
                    NamedTextColor.RED));
            return;
        }

        String targetName = pendingTargets.get(id);
        if (targetName == null
                || targetName.equalsIgnoreCase(loginServer)
                || targetName.equalsIgnoreCase(lobbyServer)) {
            event.setResult(ServerPreConnectEvent.ServerResult.allowed(event.getOriginalServer()));
            pendingTargets.remove(id);
            return;
        }

        ServerView target = registry.view(targetName);
        if (target == null) {
            pendingTargets.remove(id, targetName);
            event.setResult(ServerPreConnectEvent.ServerResult.allowed(event.getOriginalServer()));
            player.sendMessage(Component.text(
                    zh ? "「" + targetName + "」已不可用。"
                       : "« " + targetName + " » is no longer available.", NamedTextColor.YELLOW));
            return;
        }
        Optional<RegisteredServer> backend = registry.registered(targetName);
        if (target.ready() && backend.isPresent()) {
            // Keep pendingTargets until ServerConnectedEvent confirms the redirect.
            // If the connect fails, Limbo retries its lobby release and we retry too.
            event.setResult(ServerPreConnectEvent.ServerResult.allowed(backend.get()));
            return;
        }

        pendingTargets.remove(id, targetName);
        event.setResult(ServerPreConnectEvent.ServerResult.allowed(event.getOriginalServer()));
        wakeAndWaitLinked(player, targetName, false);
    }

    @Subscribe
    public void onDisconnect(DisconnectEvent event) {
        UUID id = event.getPlayer().getUniqueId();
        pendingTargets.remove(id);
        waiting.remove(id);
        lastGateNotice.remove(id);
    }

    // gateNotice shows a login-gate denial, suppressing repeats the gate's own retry
    // loop would otherwise print every few seconds.
    private void gateNotice(Player player, Component line) {
        long now = System.currentTimeMillis();
        Long last = lastGateNotice.get(player.getUniqueId());
        if (last != null && now - last < GATE_NOTICE_INTERVAL_MILLIS) {
            return;
        }
        lastGateNotice.put(player.getUniqueId(), now);
        player.sendMessage(line);
    }

    @Subscribe
    public void onServerConnected(ServerConnectedEvent event) {
        String name = event.getServer().getServerInfo().getName();
        UUID id = event.getPlayer().getUniqueId();
        pendingTargets.remove(id, name);
        if (!registry.isManaged(name)
                || name.equalsIgnoreCase(loginServer)
                || name.equalsIgnoreCase(lobbyServer)) {
            return; // system/static servers do not affect user-server activity
        }
        boolean taken = plugin.async(() -> {
            try {
                api.reportJoin(name, id);
            } catch (LinkException e) {
                // A lost join-event leaves the reaper blind to real activity and skips
                // the allowlist append, so it is an operator-visible failure.
                plugin.stats().count(ProxyStats.Event.JOIN_EVENT_FAILED);
                log.warn("Felis: join-event for {} on {} failed (status={}): {}",
                        id, name, e.statusCode(), e.getMessage());
            }
        });
        if (!taken) {
            plugin.stats().count(ProxyStats.Event.JOIN_EVENT_DROPPED);
            log.warn("Felis: join-event for {} on {} dropped: the felis-api call queue is full", id, name);
        }
    }

    /** waitingCount is how many players are parked for a backend right now. */
    int waitingCount() {
        return waiting.size();
    }

    /** tick drains the waiting queue; the plugin schedules it on the async pool. */
    void tick() {
        if (waiting.isEmpty() || !ticking.compareAndSet(false, true)) {
            return;
        }
        try {
            drain();
        } finally {
            ticking.set(false);
        }
    }

    private void drain() {
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
            boolean zh = FelisVelocityPlugin.zh(player);
            if (now > w.deadlineMillis) {
                waiting.remove(id);
                player.sendMessage(Component.text(
                        zh ? "「" + w.serverName + "」启动耗时超出预期。你可以稍后在大厅重试。"
                           : "« " + w.serverName + " » is taking longer than expected to start. "
                             + "You can try again from the lobby later.", NamedTextColor.YELLOW));
                continue;
            }
            Boolean ready = readyCache.get(w.serverName);
            if (ready == null) {
                try {
                    ServerView status = api.serverStatus(w.serverName);
                    ready = status.ready();
                    // The registry refreshes every 15 s; a server that just came up may
                    // still be registered at its old address, or not at all. Register
                    // what this poll reports before transferring anyone to it.
                    if (ready) {
                        registry.observe(status);
                    }
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
            try {
                if (!linked(id)) {
                    waiting.remove(id);
                    player.sendMessage(Component.text(
                            zh ? "你的账户已不再绑定。请重连以重新登录。"
                               : "Your account is no longer linked. Reconnect to sign in again.",
                            NamedTextColor.RED));
                    continue;
                }
            } catch (LinkException ex) {
                // Fail closed on an ambiguous identity. Keep the waiter so a later
                // tick can retry the check without losing the requested target.
                continue;
            }
            waiting.remove(id);
            player.sendMessage(Component.text(
                    zh ? "「" + w.serverName + "」已就绪——正在把你传送过去……"
                       : "« " + w.serverName + " » is ready — moving you in…", NamedTextColor.GREEN));
            // Tell a menu-driven lobby its tile is live before we pull the player off
            // it; the proxy still performs the actual Connect just below.
            MenuTransferListener listener = menuListener;
            if (w.fromMenu && listener != null) {
                listener.onReady(player, w.serverName);
            }
            transfer(player, w.serverName, backend.get());
        }
    }

    private void authorizeAndWait(Player player, String serverName, boolean fromMenu) {
        UUID id = player.getUniqueId();
        boolean zh = FelisVelocityPlugin.zh(player);
        // Asking for the server you are standing on is a no-op, and it has to be caught
        // here rather than at each entry point: the menu tile, /felis go and an accepted
        // invite all funnel through this method. Without the guard the request runs the
        // whole wake-and-queue path and ends in a Connect to the current server, which
        // Velocity answers ALREADY_CONNECTED — the player sees a failure for something
        // that was never a real move. Cheapest for the lobby, where the tile for the
        // lobby itself sits in front of every player who is already in it.
        if (player.getCurrentServer()
                .map(sc -> sc.getServerInfo().getName().equalsIgnoreCase(serverName))
                .orElse(false)) {
            player.sendMessage(Component.text(
                    zh ? "你已经在「" + serverName + "」了。"
                       : "You're already on « " + serverName + " ».", NamedTextColor.YELLOW));
            return;
        }
        plugin.async(player, () -> {
            try {
                if (!linked(id)) {
                    player.sendMessage(Component.text(
                            zh ? "请先完成登录，再加入服务器。"
                               : "Finish signing in before joining a server.", NamedTextColor.YELLOW));
                    return;
                }
            } catch (LinkException e) {
                log.warn("Felis: could not verify queue entry for {} (status={}): {}",
                        id, e.statusCode(), e.getMessage());
                player.sendMessage(Component.text(
                        zh ? "登录验证暂时不可用，请稍后重试。"
                           : "Login verification is temporarily unavailable. Please try again shortly.",
                        NamedTextColor.RED));
                return;
            }
            if (joinIfRunning(player, serverName, fromMenu)) {
                return;
            }
            wakeAndWaitLinked(player, serverName, fromMenu);
        });
    }

    /**
     * joinIfRunning is the ready-or-wake split of the host path, for the entries that
     * start from inside the proxy: a server a fresh status poll reports up is joined
     * directly, at the address the poll carries.
     *
     * <p>A running felis server is already reachable by any linked player through
     * {@code <name>.<root-domain>}, which {@link #onServerPreConnect} admits on the link
     * check alone, so joining one grants nothing. Waking it instead asks the API to
     * start a server that needs no starting, and autostartPolicy defaults to ownerOnly:
     * a friend's green "join" tile answered "you're not allowed to start it". WAKING a
     * stopped server is still a start, and still policy-gated. A poll that fails falls
     * through to the wake, which reports the failure its own way.
     */
    private boolean joinIfRunning(Player player, String serverName, boolean fromMenu) {
        ServerView status;
        try {
            status = api.serverStatus(serverName);
        } catch (LinkException e) {
            return false;
        }
        if (!status.ready()) {
            return false;
        }
        registry.observe(status);
        Optional<RegisteredServer> backend = registry.registered(serverName);
        if (backend.isEmpty()) {
            return false; // up, but not listed or addressed yet → the queue waits for it
        }
        MenuTransferListener listener = menuListener;
        if (fromMenu && listener != null) {
            listener.onReady(player, serverName);
        }
        transfer(player, serverName, backend.get());
        return true;
    }

    // Caller already ran the authoritative link-status check and is off the event
    // thread. Keep the wake and queue mutation together so every entry has passed
    // both the account gate and the server-side autostart policy.
    private void wakeAndWaitLinked(Player player, String serverName, boolean fromMenu) {
        UUID id = player.getUniqueId();
        boolean zh = FelisVelocityPlugin.zh(player);
        boolean up = false;
        try {
            // An up server answers ready without waking anything; the queue still
            // does the move, so the player just is not told it is starting.
            up = api.wake(serverName, id).ready();
        } catch (LinkException e) {
            switch (e.statusCode()) {
                case 403:
                    player.sendMessage(Component.text(
                            zh ? "你无权启动「" + serverName + "」。"
                               : "You're not allowed to start « " + serverName + " ».", NamedTextColor.RED));
                    return;
                case 409:
                    if ("maintenance_in_progress".equals(e.errorCode())) {
                        // A restore, backup or file write owns the world right now and
                        // the server will not start until it finishes (minutes at most),
                        // so waiting here would only run into the queue timeout.
                        player.sendMessage(Component.text(
                                zh ? "「" + serverName + "」正在维护（回档、备份或改文件），请稍后再试。"
                                   : "« " + serverName + " » is under maintenance (restore, backup or file edit)."
                                     + " Please try again shortly.",
                                NamedTextColor.YELLOW));
                        return;
                    }
                    logWakeFailure(player, serverName, zh, e);
                    return;
                case 429:
                    break; // a wake is already in flight → join the existing wait
                case 503:
                    if ("at_capacity".equals(e.errorCode())) {
                        // at_capacity: nothing is coming up, so enqueueing would only strand
                        // the player until the timeout. Be honest and let them retry later.
                        player.sendMessage(Component.text(
                                zh ? "集群当前已满——「" + serverName + "」暂时无法启动。请稍后再试。"
                                   : "The cluster is at capacity right now — « " + serverName
                                     + " » can't start. Please try again later.",
                                NamedTextColor.YELLOW));
                        return;
                    }
                    // A 503 without the at_capacity code is a plain outage, not a
                    // capacity verdict — report it like any other failure.
                    logWakeFailure(player, serverName, zh, e);
                    return;
                default:
                    logWakeFailure(player, serverName, zh, e);
                    return;
            }
        }
        if (!up) {
            player.sendMessage(Component.text(
                    zh ? "正在启动「" + serverName + "」——就绪后会自动把你传送过去。"
                       : "Starting « " + serverName + " » — you'll be moved in automatically.",
                    NamedTextColor.GRAY));
        }
        waiting.put(id, new Waiter(
                serverName, System.currentTimeMillis() + WAIT_TIMEOUT_MILLIS, fromMenu));
    }

    private void logWakeFailure(Player player, String serverName, boolean zh, LinkException e) {
        log.warn("Felis: wake {} failed (status={}): {}", serverName, e.statusCode(), e.getMessage());
        player.sendMessage(Component.text(
                zh ? "现在无法启动「" + serverName + "」。请稍后再试。"
                   : "Couldn't start « " + serverName + " » right now. Try again shortly.",
                NamedTextColor.RED));
    }

    private void transfer(Player player, String serverName, RegisteredServer backend) {
        player.createConnectionRequest(backend).connect().whenComplete((result, err) -> {
            if (err != null || (result != null && !result.isSuccessful())) {
                plugin.stats().count(ProxyStats.Event.TRANSFER_FAILED);
                log.warn("Felis: transfer of {} to {} failed: {}", player.getUniqueId(), serverName,
                        err != null ? err.toString() : result.getStatus());
                boolean zh = FelisVelocityPlugin.zh(player);
                Component line = Component.text(
                        zh ? "无法把你连接到「" + serverName + "」。"
                           : "Couldn't connect you to « " + serverName + " ».",
                        NamedTextColor.RED);
                // A backend that refuses the login says why (ban, whitelist, full, mods);
                // "please try again" is wrong advice for all of those.
                Optional<Component> reason = result == null ? Optional.empty() : result.getReasonComponent();
                line = reason.isPresent()
                        ? line.append(Component.text(zh ? " 原因：" : " Reason: ", NamedTextColor.RED))
                              .append(reason.get())
                        : line.append(Component.text(zh ? "请重试。" : " Please try again.", NamedTextColor.RED));
                player.sendMessage(line);
            }
        });
    }

    private Optional<RegisteredServer> login() {
        return proxy.getServer(loginServer);
    }

    private static boolean serverNamed(RegisteredServer server, String name) {
        return server.getServerInfo().getName().equalsIgnoreCase(name);
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
