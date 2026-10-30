package best.lolicon.felis.velocity;

import best.lolicon.felis.link.FelisApiClient;
import best.lolicon.felis.link.EntryPolicy;
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
import java.util.Set;
import java.util.UUID;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.function.LongSupplier;

/** Host-based routing and the waiting queue: login first, then the requested backend, or the lobby while it wakes. */
public final class WaitingRouter {
    private static final long WAIT_TIMEOUT_MILLIS = 120_000L; // renewed by each poll that shows the server still coming up
    private static final long MAX_WAIT_MILLIS = 60 * 60_000L; // backstop for a start whose status never moves
    private static final long PROGRESS_NOTICE_MILLIS = 60_000L;
    private static final String DESIRED_STOPPED = "Stopped";
    private static final long LINK_GRACE_MILLIS = 10 * 60_000L; // a positive link answer may stand in for felis-api this long
    private static final long GATE_NOTICE_INTERVAL_MILLIS = 15_000L; // the gate retries its release; show a denial once per interval
    private static final int TRANSFER_ATTEMPTS = 3; // dials into a server that fails the connect without a reason

    private final ProxyServer proxy;
    private final Logger log;
    private final FelisApiClient api;
    private final ServerRegistry registry;
    private final FelisVelocityPlugin plugin;
    private final String loginServer;
    private final String lobbyServer;

    private volatile EntryPolicy entryPolicy = EntryPolicy.legacy();
    private final Map<UUID, EntryPolicy> entrySessions = new ConcurrentHashMap<>();

    void setEntryPolicy(EntryPolicy policy) { this.entryPolicy = policy; }

    private final LinkGate links;
    private final Map<UUID, Waiter> waiting = new ConcurrentHashMap<>();
    private final Map<UUID, String> pendingTargets = new ConcurrentHashMap<>();
    private final Map<UUID, Long> lastGateNotice = new ConcurrentHashMap<>();
    private final Set<UUID> authorizing = ConcurrentHashMap.newKeySet(); // login releases and queue entries mid-authorization; drops the gate's repeated sends
    private final Map<UUID, String> exits = new ConcurrentHashMap<>(); // without a lobby: the one server a released player may leave the login space for
    private final AtomicBoolean ticking = new AtomicBoolean(false); // overlapping ticks would add load while felis-api is slow

    private volatile MenuTransferListener menuListener; // null until ControlChannel is wired in at init; read on the tick pool
    private volatile LongSupplier clock = System::currentTimeMillis; // tests move it to walk a long start

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

    void setClock(LongSupplier clock) {
        this.clock = clock;
    }

    void pruneLinks() {
        links.prune();
    }

    /** Moves the player to the lobby; {@link #onServerPreConnect} authorizes it, so an early release is only denied. */
    void releaseFromLogin(Player player) {
        if (lobbyServer.isEmpty()) {
            UUID id = player.getUniqueId();
            if (waiting.containsKey(id) || exits.containsKey(id) || !authorizing.add(id)) {
                return; // already leaving, waiting, or being decided: the gate's repeats are dropped
            }
            if (!plugin.async(() -> {
                try {
                    releaseWithoutLobby(player);
                } finally {
                    authorizing.remove(id);
                }
            })) {
                authorizing.remove(id);
            }
            return;
        }
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

    /** Drops a refused redirect target (gate retries would hit the same refusal), shows its reason, then releases to the lobby. */
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

    // Without a lobby the router picks the exit itself: the ready target, the fallback, or a wait in the login space.
    private void releaseWithoutLobby(Player player) {
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
        if (targetName == null) {
            entryDisconnect(player, "登录已完成。此地址没有对应的服务器，请使用服务器的连接地址重新连接。",
                    "Signed in. This address leads to no server; reconnect using the server's own address.");
            return;
        }
        ServerView target;
        try {
            target = api.serverStatus(targetName);
        } catch (LinkException e) {
            if (e.statusCode() == 404) {
                pendingTargets.remove(id, targetName);
                entryDisconnect(player, "「" + targetName + "」已不可用。", "« " + targetName + " » is no longer available.");
            } else {
                log.warn("Felis: login release for {} could not read {}: {}", id, targetName, e.getMessage());
            }
            return; // an outage leaves the gate asking again
        }
        registry.observe(target);
        Optional<RegisteredServer> backend = registry.registered(targetName);
        if (target.ready() && backend.isPresent()) {
            transfer(player, targetName, backend.get(), false, 1);
            return;
        }
        EntryPolicy policy = entrySessions.getOrDefault(id, entryPolicy);
        if (policy.offlineAction().equals("disconnect")) {
            entryDisconnect(player, "目标服务器当前未运行，请稍后重连。", "The destination is offline. Reconnect later.");
            return;
        }
        if (policy.offlineAction().equals("fallback")) {
            try {
                ServerView fallback = api.serverStatus(policy.fallbackServer());
                registry.observe(fallback);
                Optional<RegisteredServer> destination = registry.registered(fallback.name());
                if (fallback.ready() && destination.isPresent()) {
                    pendingTargets.remove(id);
                    transfer(player, fallback.name(), destination.get(), false, 1);
                    return;
                }
            } catch (LinkException error) {
                log.warn("Felis: fallback lookup failed: {}", error.getMessage());
            }
            entryDisconnect(player, "备用服务器不可用，请稍后重连。", "The fallback server is unavailable. Reconnect later.");
            return;
        }
        pendingTargets.remove(id, targetName);
        wakeAndWaitLinked(player, targetName, false);
        if (!waiting.containsKey(id)) {
            entryDisconnect(player, "服务器无法自动启动，请联系管理员检查启动权限、维护状态及资源。",
                    "Automatic startup was refused. Contact the administrator to check permissions, maintenance and capacity.");
        }
    }

    private boolean linked(UUID id) throws LinkException {
        EntryPolicy policy = entrySessions.get(id);
        if (policy != null && !policy.requireAccountLink()) {
            return !api.isBlacklisted(id);
        }
        LinkGate.Result r = links.check(id);
        if (r.degraded) {
            log.warn("Felis: felis-api unreachable; admitting {} on a link confirmation from the last {} min",
                    id, LINK_GRACE_MILLIS / 60_000L);
        }
        return r.linked;
    }

    void setMenuTransferListener(MenuTransferListener listener) {
        this.menuListener = listener;
    }

    /** Moves a player already on the proxy to the server picked in the lobby menu; fires {@link MenuTransferListener} on transfer. */
    void enqueueFromMenu(Player player, String serverName) {
        authorizeAndWait(player, serverName, true);
    }

    /** Like {@link #enqueueFromMenu} for /felis go: no menu notification; a wake stays policy-gated on the verified UUID. */
    void enqueueFromCommand(Player player, String serverName) {
        authorizeAndWait(player, serverName, false);
    }

    /** Moves a player past the login gate back to the lobby; nothing to wake, and a wait already joined stays. */
    void toLobby(Player player) {
        boolean zh = FelisVelocityPlugin.zh(player);
        if (lobbyServer.isEmpty()) {
            player.sendMessage(Component.text(
                    zh ? "此平台没有大厅。" : "This platform has no lobby.", NamedTextColor.YELLOW));
            return;
        }
        if (player.getCurrentServer()
                .map(sc -> serverNamed(sc.getServer(), lobbyServer))
                .orElse(false)) {
            player.sendMessage(Component.text(
                    zh ? "你已经在大厅了。" : "You're already in the lobby.", NamedTextColor.YELLOW));
            return;
        }
        Optional<RegisteredServer> lobby = proxy.getServer(lobbyServer);
        if (lobby.isEmpty()) {
            log.warn("Felis: {} asked for the lobby but '{}' is not registered", player.getUniqueId(), lobbyServer);
            player.sendMessage(Component.text(
                    zh ? "大厅暂时不可用，请稍后再试。" : "The lobby is unavailable right now. Try again shortly.",
                    NamedTextColor.RED));
            return;
        }
        // single dial: the lobby is not a queue target
        transfer(player, lobbyServer, lobby.get(), false, TRANSFER_ATTEMPTS);
    }

    /** Like {@link #enqueueFromCommand} for an accepted invite; the target is already running. */
    void enqueueFromInvite(Player player, String serverName) {
        authorizeAndWait(player, serverName, false);
    }

    @Subscribe
    public EventTask onChooseInitialServer(PlayerChooseInitialServerEvent event) {
        Player player = event.getPlayer();
        UUID id = player.getUniqueId();
        entrySessions.remove(id);
        waiting.remove(id);
        EntryPolicy policy = entryPolicy;
        if (!policy.requireAccountLink()) {
            return EventTask.async(() -> chooseAuthenticated(event, policy));
        }
        entrySessions.put(id, policy);
        pendingTargets.remove(id); // a reconnect must never inherit an earlier host
        Optional<ServerView> targetOpt = policyTarget(player, policy);
        if (targetOpt.isEmpty()) {
            return null; // unknown host also falls through to login
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
            return null;
        }
        // login is the gate, not a destination: remembering it would loop the lobby release back into login
        if (!target.name().equalsIgnoreCase(loginServer)) {
            pendingTargets.put(id, target.name());
        }
        event.setInitialServer(login.get());
        return null;
    }

    private Optional<ServerView> policyTarget(Player player, EntryPolicy policy) {
        if (policy.mode().equals("lobby")) return Optional.ofNullable(registry.view(lobbyServer));
        if (policy.mode().equals("domain")) {
            Optional<ServerView> host = virtualHost(player).flatMap(registry::resolveByHost)
                    .filter(server -> !server.name().equalsIgnoreCase(loginServer));
            if (host.isPresent()) return host;
        }
        return Optional.ofNullable(registry.view(policy.defaultServer()));
    }

    private void chooseAuthenticated(PlayerChooseInitialServerEvent event, EntryPolicy policy) {
        Player player = event.getPlayer();
        event.setInitialServer(null);
        pendingTargets.remove(player.getUniqueId());
        try {
            if (api.isBlacklisted(player.getUniqueId())) {
                entryDisconnect(player, "此游戏身份已被禁止登录。", "This game identity is barred.");
                return;
            }
            entrySessions.put(player.getUniqueId(), policy);
            ServerView target = policyTarget(player, policy).orElse(null);
            if (target == null) {
                entryDisconnect(player, "未配置此入口的目标服务器，请联系管理员。", "No destination is configured for this entry. Contact the administrator.");
                return;
            }
            // Refresh readiness instead of routing on the registration cache alone.
            target = api.serverStatus(target.name());
            registry.observe(target);
            if (target.ready() && registry.registered(target.name()).isPresent()) {
                event.setInitialServer(registry.registered(target.name()).get());
                return;
            }
            if (policy.offlineAction().equals("fallback")) {
                ServerView fallback = api.serverStatus(policy.fallbackServer());
                registry.observe(fallback);
                if (fallback.ready() && registry.registered(fallback.name()).isPresent()) {
                    event.setInitialServer(registry.registered(fallback.name()).get());
                    return;
                }
                entryDisconnect(player, "目标服务器与备用服务器均不可用。", "The destination and fallback server are unavailable.");
                return;
            }
            if (policy.offlineAction().equals("disconnect")) {
                entryDisconnect(player, "目标服务器当前未运行，请稍后重连。", "The destination is offline. Reconnect later.");
                return;
            }
            String space = policy.waitingSpace().equals("login") || lobbyServer.isEmpty() ? loginServer : lobbyServer;
            ServerView spaceStatus = api.serverStatus(space);
            registry.observe(spaceStatus);
            Optional<RegisteredServer> waitingSpace = proxy.getServer(space);
            if (!spaceStatus.ready() || waitingSpace.isEmpty()) {
                entryDisconnect(player, "等待空间不可用，请联系管理员。", "The waiting space is unavailable. Contact the administrator.");
                return;
            }
            wakeAndWaitLinked(player, target.name(), false);
            if (!waiting.containsKey(player.getUniqueId())) {
                entryDisconnect(player, "服务器无法自动启动，请联系管理员检查启动权限、维护状态及资源。", "Automatic startup was refused. Contact the administrator to check permissions, maintenance and capacity.");
                return;
            }
            event.setInitialServer(waitingSpace.get());
        } catch (LinkException error) {
            log.warn("Felis: entry routing failed for {}: {}", player.getUniqueId(), error.getMessage());
            entryDisconnect(player, "身份或路由服务暂不可用，请稍后重连。", "Identity or routing service is unavailable. Reconnect later.");
        }
    }

    private static void entryDisconnect(Player player, String zh, String en) {
        player.disconnect(Component.text(FelisVelocityPlugin.zh(player) ? zh : en, NamedTextColor.RED));
    }

    // Recheck linking at the proxy boundary before honoring login's release request.
    @Subscribe
    public EventTask onServerPreConnect(ServerPreConnectEvent event) {
        EntryPolicy session = entrySessions.get(event.getPlayer().getUniqueId());
        if (session != null && !session.requireAccountLink()) return null;
        RegisteredServer previous = event.getPreviousServer();
        if (previous == null || !serverNamed(previous, loginServer)) {
            return null;
        }

        Player player = event.getPlayer();
        event.setResult(ServerPreConnectEvent.ServerResult.denied());
        if (lobbyServer.isEmpty()) {
            String exit = exits.get(player.getUniqueId());
            if (exit != null && serverNamed(event.getOriginalServer(), exit)) {
                event.setResult(ServerPreConnectEvent.ServerResult.allowed(event.getOriginalServer()));
                return null;
            }
            player.sendMessage(Component.text(
                    FelisVelocityPlugin.zh(player)
                            ? "登录网关只能把玩家放行到其目标服务器。"
                            : "The login gate may only release players to their own server.",
                    NamedTextColor.RED));
            log.warn("Felis: denied login-gate transfer for {} to {}",
                    player.getUniqueId(), event.getOriginalServer().getServerInfo().getName());
            return null;
        }
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

        UUID id = player.getUniqueId();
        if (!authorizing.add(id)) {
            return null; // the release already being authorized answers; the gate asks again after
        }
        return EventTask.async(() -> {
            try {
                authorizeLoginRelease(event);
            } finally {
                authorizing.remove(id);
            }
        });
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
            // Keep pendingTargets until ServerConnectedEvent confirms the redirect. If the connect fails, Limbo retries its lobby release and so does this router.
            event.setResult(ServerPreConnectEvent.ServerResult.allowed(backend.get()));
            return;
        }

        EntryPolicy policy = entrySessions.getOrDefault(id, EntryPolicy.legacy());
        if (policy.offlineAction().equals("disconnect")) {
            entryDisconnect(player, "目标服务器当前未运行，请稍后重连。", "The destination is offline. Reconnect later.");
            return;
        }
        if (policy.offlineAction().equals("fallback")) {
            try {
                ServerView fallback = api.serverStatus(policy.fallbackServer());
                registry.observe(fallback);
                Optional<RegisteredServer> destination = registry.registered(fallback.name());
                if (fallback.ready() && destination.isPresent()) {
                    pendingTargets.remove(id);
                    event.setResult(ServerPreConnectEvent.ServerResult.allowed(destination.get()));
                    return;
                }
            } catch (LinkException error) {
                log.warn("Felis: fallback lookup failed: {}", error.getMessage());
            }
            entryDisconnect(player, "备用服务器不可用，请稍后重连。", "The fallback server is unavailable. Reconnect later.");
            return;
        }
        pendingTargets.remove(id, targetName);
        event.setResult(ServerPreConnectEvent.ServerResult.allowed(event.getOriginalServer()));
        wakeAndWaitLinked(player, targetName, false);
    }

    @Subscribe
    public void onDisconnect(DisconnectEvent event) {
        UUID id = event.getPlayer().getUniqueId();
        exits.remove(id);
        pendingTargets.remove(id);
        entrySessions.remove(id);
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
        exits.remove(id, name);
        EntryPolicy session = entrySessions.get(id);
        Waiter waiter = waiting.get(id);
        if (session != null && !session.requireAccountLink() && waiter != null) {
            event.getPlayer().sendMessage(Component.text(
                    FelisVelocityPlugin.zh(event.getPlayer())
                            ? "正在等待「" + waiter.serverName + "」启动。服务器就绪后将自动连接。"
                            : "Waiting for « " + waiter.serverName + " » to start. Connection proceeds automatically when ready.",
                    NamedTextColor.GRAY));
        }
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
        long now = clock.getAsLong();
        Map<String, Optional<ServerView>> polled = new HashMap<>(); // one status poll per distinct server
        for (Map.Entry<UUID, Waiter> e : new ArrayList<>(waiting.entrySet())) {
            UUID id = e.getKey();
            Waiter w = e.getValue();
            Optional<Player> po = proxy.getPlayer(id);
            if (po.isEmpty()) {
                waiting.remove(id); // player left the proxy
                continue;
            }
            Player player = po.get();
            EntryPolicy session = entrySessions.get(id);
            if (session != null && !session.requireAccountLink() && player.getCurrentServer().isEmpty()) continue;
            boolean zh = FelisVelocityPlugin.zh(player);
            Optional<ServerView> poll = polled.get(w.serverName);
            if (poll == null) {
                poll = poll(w.serverName);
                polled.put(w.serverName, poll);
            }
            ServerView status = poll.orElse(null);
            if (status != null && status.ready() && now - w.sinceMillis > MAX_WAIT_MILLIS) {
                waiting.remove(id);
                disconnectAutomaticWait(player);
                player.sendMessage(Component.text("服务器等待超时 / Server waiting timed out", NamedTextColor.RED));
                continue;
            }
            if (status == null || !status.ready()) {
                if (status != null && !stillComing(player, zh, w, status, now)) {
                    waiting.remove(id);
                    disconnectAutomaticWait(player);
                } else if (now > w.deadlineMillis || now - w.sinceMillis > MAX_WAIT_MILLIS) {
                    waiting.remove(id);
                    disconnectAutomaticWait(player);
                    player.sendMessage(Component.text(
                            lobbyServer.isEmpty()
                                    ? (zh ? "「" + w.serverName + "」启动耗时超出预期，请稍后重新连接。"
                                          : "« " + w.serverName + " » is taking longer than expected to start. "
                                            + "Reconnect later.")
                                    : (zh ? "「" + w.serverName + "」启动耗时超出预期。你可以稍后在大厅重试。"
                                          : "« " + w.serverName + " » is taking longer than expected to start. "
                                            + "You can try again from the lobby later."), NamedTextColor.YELLOW));
                }
                continue;
            }
            Optional<RegisteredServer> backend = registry.registered(w.serverName);
            if (backend.isEmpty()) {
                continue; // ready but not yet registered → next tick
            }
            try {
                if (!linked(id)) {
                    waiting.remove(id);
                    if (session != null && !session.requireAccountLink()) {
                        entryDisconnect(player, "此游戏身份已被禁止登录。", "This game identity is barred.");
                        continue;
                    }
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
            // Tell a menu-driven lobby its tile is live before the player is pulled off it; the proxy still performs the actual Connect just below.
            MenuTransferListener listener = menuListener;
            if (w.fromMenu && listener != null) {
                listener.onReady(player, w.serverName);
            }
            transfer(player, w.serverName, backend.get(), w.fromMenu, w.attempts + 1);
        }
    }

    private void disconnectAutomaticWait(Player player) {
        disconnectAutomaticWait(player, Component.text(FelisVelocityPlugin.zh(player)
                ? "服务器启动失败或等待超时，请联系管理员检查启动日志。"
                : "Startup failed or waiting timed out. Contact the administrator to inspect the startup log.",
                NamedTextColor.RED));
    }

    // A failed wait ends the connection of a player parked in the login space, which is no place to stay.
    private void disconnectAutomaticWait(Player player, Component reason) {
        EntryPolicy policy = entrySessions.get(player.getUniqueId());
        boolean parked = lobbyServer.isEmpty() || (policy != null && !policy.requireAccountLink());
        if (parked && player.getCurrentServer()
                .map(server -> serverNamed(server.getServer(), loginServer)).orElse(false)) {
            player.disconnect(reason);
        }
    }

    // poll asks felis-api how one waited-on server is doing; empty when it does not
    // answer, which the waiters ride out until their window closes.
    private Optional<ServerView> poll(String serverName) {
        try {
            ServerView status = api.serverStatus(serverName);
            // The registry refreshes every 15 s; a server that just came up may still
            // be registered at its old address, or not at all. Register what this poll
            // reports before transferring anyone to it.
            if (status.ready()) {
                registry.observe(status);
            }
            return Optional.of(status);
        } catch (LinkException ex) {
            return Optional.empty();
        }
    }

    // Renew waits while startup progresses; stop waiting after cancellation or exhausted retries.
    private boolean stillComing(Player player, boolean zh, Waiter w, ServerView status, long now) {
        if (status.startGaveUp()) {
            tellStartFailed(player, zh, w.serverName);
            return false;
        }
        if (DESIRED_STOPPED.equalsIgnoreCase(status.desiredState())) {
            // felis-api reads servers from an informer cache, so the first poll after
            // the wake can still show the old desired state; two in a row are a stop.
            if (w.stopSeen) {
                player.sendMessage(Component.text(
                        zh ? "「" + w.serverName + "」已被停止，不再为你排队。"
                           : "« " + w.serverName + " » was stopped, so you're no longer waiting for it.",
                        NamedTextColor.YELLOW));
                return false;
            }
            w.stopSeen = true;
            return true;
        }
        w.stopSeen = false;
        w.deadlineMillis = now + WAIT_TIMEOUT_MILLIS;
        if (status.autoRestarts() > w.restartsSeen) {
            w.restartsSeen = status.autoRestarts();
            w.noticedMillis = now;
            player.sendMessage(Component.text(
                    zh ? "「" + w.serverName + "」启动超时，正在自动重试（第 " + w.restartsSeen + " 次）……"
                       : "« " + w.serverName + " » timed out starting; retrying automatically (attempt "
                         + w.restartsSeen + ")…",
                    NamedTextColor.YELLOW));
        } else if (now - w.noticedMillis >= PROGRESS_NOTICE_MILLIS) {
            w.noticedMillis = now;
            long minutes = (now - w.sinceMillis) / 60_000L;
            player.sendMessage(Component.text(
                    zh ? "「" + w.serverName + "」仍在启动（已等 " + minutes + " 分钟），就绪后会自动把你传送过去。"
                       : "« " + w.serverName + " » is still starting (" + minutes + " min so far); "
                         + "you'll be moved in when it's ready.",
                    NamedTextColor.GRAY));
        }
        return true;
    }

    private void authorizeAndWait(Player player, String serverName, boolean fromMenu) {
        UUID id = player.getUniqueId();
        boolean zh = FelisVelocityPlugin.zh(player);
        // Joining the current server is a no-op across menu, command, and invite paths.
        if (player.getCurrentServer()
                .map(sc -> sc.getServerInfo().getName().equalsIgnoreCase(serverName))
                .orElse(false)) {
            player.sendMessage(Component.text(
                    zh ? "你已经在「" + serverName + "」了。"
                       : "You're already on « " + serverName + " ».", NamedTextColor.YELLOW));
            return;
        }
        // A wait that is already on for this server answers a second click itself: going
        // round again would only wake the server once more and restart the wait.
        Waiter queued = waiting.get(id);
        if (queued != null && queued.serverName.equalsIgnoreCase(serverName)) {
            player.sendMessage(Component.text(
                    zh ? "你已在排队等待「" + queued.serverName + "」，就绪后会自动把你传送过去。"
                       : "You're already waiting for « " + queued.serverName + " » — you'll be moved in when it's ready.",
                    NamedTextColor.GRAY));
            return;
        }
        if (!authorizing.add(id)) {
            player.sendMessage(Component.text(
                    zh ? "上一个请求还在处理，请稍候。" : "Still working on your last request — hold on a moment.",
                    NamedTextColor.GRAY));
            return;
        }
        boolean taken = plugin.async(player, () -> {
            try {
                authorizeEntry(player, zh, serverName, fromMenu);
            } finally {
                authorizing.remove(id);
            }
        });
        if (!taken) {
            authorizing.remove(id);
        }
    }

    private void authorizeEntry(Player player, boolean zh, String serverName, boolean fromMenu) {
        UUID id = player.getUniqueId();
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
    }

    // Join running servers directly; stopped servers still require policy-gated wake admission.
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
        transfer(player, serverName, backend.get(), fromMenu, 1);
        return true;
    }

    // Keep wake admission and queue insertion together after the authoritative link check.
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
                    if ("world_reclaiming".equals(e.errorCode())) {
                        // The idle reaper is archiving the world. When it is done the
                        // server is released with an empty world, so there is nothing
                        // to wait for, and "try again shortly" would mislead.
                        player.sendMessage(Component.text(
                                zh ? "「" + serverName + "」闲置太久，世界正在归档回收，现在不能启动。"
                                     + "回收后它会变成可认领的空服，旧世界留在归档里。"
                                   : "« " + serverName + " » sat idle too long and its world is being archived, so it can't start."
                                     + " Afterwards it becomes a claimable empty server; the old world stays in the archive.",
                                NamedTextColor.YELLOW));
                        return;
                    }
                    if ("server_retiring".equals(e.errorCode())) {
                        // The owner gave the server up or an admin is deleting it: it
                        // stays down until the reaper archives it, unless that is
                        // cancelled in the panel, so there is nothing to wait for.
                        player.sendMessage(Component.text(
                                zh ? "「" + serverName + "」已被放弃或正在删除，不能启动。"
                                   : "« " + serverName + " » has been given up or is being deleted, so it can't be started.",
                                NamedTextColor.YELLOW));
                        return;
                    }
                    if ("start_failed".equals(e.errorCode())) {
                        // The last start failed with its retries spent. The join does
                        // not buy another round of them, so there is nothing to wait for.
                        tellStartFailed(player, zh, serverName);
                        return;
                    }
                    logWakeFailure(player, serverName, zh, e);
                    return;
                case 429:
                    break; // a wake is already in flight → join the existing wait
                case 503:
                    if ("at_capacity".equals(e.errorCode())) {
                        // at_capacity: nothing is coming up, so enqueueing would only strand the player until the timeout. Tell them to retry later.
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
        waiting.put(id, new Waiter(serverName, clock.getAsLong(), fromMenu, 0));
    }

    // The server's start failed and its automatic retries are spent: nothing more is
    // coming until a person starts it again from the panel.
    private static void tellStartFailed(Player player, boolean zh, String serverName) {
        player.sendMessage(Component.text(
                zh ? "「" + serverName + "」启动失败，自动重试也已用完。服主可以在面板查看日志后重新启动。"
                   : "« " + serverName + " » failed to start and its automatic retries are spent. "
                     + "The owner can check its log in the panel and start it again.",
                NamedTextColor.RED));
    }

    private void logWakeFailure(Player player, String serverName, boolean zh, LinkException e) {
        log.warn("Felis: wake {} failed (status={}): {}", serverName, e.statusCode(), e.getMessage());
        player.sendMessage(Component.text(
                zh ? "现在无法启动「" + serverName + "」。请稍后再试。"
                   : "Couldn't start « " + serverName + " » right now. Try again shortly.",
                NamedTextColor.RED));
    }

    // Retry unexplained connection failures only; explicit refusal or preemption terminates transfer immediately.
    private void transfer(Player player, String serverName, RegisteredServer backend,
                          boolean fromMenu, int attempt) {
        if (lobbyServer.isEmpty()) {
            exits.put(player.getUniqueId(), serverName);
        }
        player.createConnectionRequest(backend).connect().whenComplete((result, err) -> {
            if (err == null && (result == null || result.isSuccessful())) {
                return;
            }
            plugin.stats().count(ProxyStats.Event.TRANSFER_FAILED);
            log.warn("Felis: transfer of {} to {} failed (attempt {}/{}): {}", player.getUniqueId(), serverName,
                    attempt, TRANSFER_ATTEMPTS, err != null ? err.toString() : result.getStatus());
            boolean zh = FelisVelocityPlugin.zh(player);
            boolean unexplained = err != null
                    || (result.getStatus() == ConnectionRequestBuilder.Status.SERVER_DISCONNECTED
                        && result.getReasonComponent().isEmpty());
            // putIfAbsent: a player who queued for another server meanwhile keeps that wait.
            if (unexplained && attempt < TRANSFER_ATTEMPTS
                    && waiting.putIfAbsent(player.getUniqueId(),
                            new Waiter(serverName, clock.getAsLong(), fromMenu, attempt)) == null) {
                player.sendMessage(Component.text(
                        zh ? "没能连上「" + serverName + "」，稍后自动重试……"
                           : "Couldn't reach « " + serverName + " »; trying again shortly…",
                        NamedTextColor.YELLOW));
                return;
            }
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
            disconnectAutomaticWait(player, line);
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
        final boolean fromMenu; // true → notify the felis:control face on transfer
        final long sinceMillis;
        final int attempts; // dials into serverName already made and failed
        // Only the drain touches these, one tick at a time (the ticking flag orders
        // the ticks), so they need no further synchronization.
        long deadlineMillis;
        long noticedMillis;
        int restartsSeen;
        boolean stopSeen;

        Waiter(String serverName, long nowMillis, boolean fromMenu, int attempts) {
            this.serverName = serverName;
            this.fromMenu = fromMenu;
            this.attempts = attempts;
            this.sinceMillis = nowMillis;
            this.deadlineMillis = nowMillis + WAIT_TIMEOUT_MILLIS;
            this.noticedMillis = nowMillis;
        }
    }

    interface MenuTransferListener {
        void onReady(Player player, String serverName);
    }
}
