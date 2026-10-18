package best.lolicon.felis.velocity;

import best.lolicon.felis.link.FelisApiClient;
import best.lolicon.felis.link.LinkClient;
import best.lolicon.felis.link.LinkCode;
import best.lolicon.felis.link.LinkException;
import best.lolicon.felis.link.OpLoginView;
import best.lolicon.felis.link.OutageTracker;
import best.lolicon.felis.link.ServerView;

import com.google.inject.Inject;
import com.mojang.brigadier.Command;
import com.mojang.brigadier.arguments.StringArgumentType;
import com.mojang.brigadier.tree.LiteralCommandNode;
import com.velocitypowered.api.command.BrigadierCommand;
import com.velocitypowered.api.command.CommandManager;
import com.velocitypowered.api.command.CommandMeta;
import com.velocitypowered.api.command.CommandSource;
import com.velocitypowered.api.event.Subscribe;
import com.velocitypowered.api.event.connection.DisconnectEvent;
import com.velocitypowered.api.event.proxy.ProxyInitializeEvent;
import com.velocitypowered.api.event.proxy.ProxyShutdownEvent;
import com.velocitypowered.api.plugin.Plugin;
import com.velocitypowered.api.plugin.PluginManager;
import com.velocitypowered.api.plugin.annotation.DataDirectory;
import com.velocitypowered.api.proxy.Player;
import com.velocitypowered.api.proxy.ProxyServer;
import com.velocitypowered.api.proxy.ServerConnection;
import net.kyori.adventure.text.Component;
import net.kyori.adventure.text.format.NamedTextColor;
import org.slf4j.Logger;

import java.nio.file.Path;
import java.time.Duration;
import java.time.Instant;
import java.util.List;
import java.util.Locale;
import java.util.Optional;
import java.util.UUID;
import java.util.concurrent.atomic.AtomicLong;
import java.util.regex.Pattern;

/** Velocity side of Felis: /link, /felis, /invite and subdomain routing; routing needs online mode and a root-domain. */
@Plugin(
        id = "felis-link",
        name = "Felis Velocity",
        version = "0.2.0",
        description = "In-game /link plus domain-autostart routing: recognizes server subdomains, "
                + "registers backends, wakes sleeping servers and holds players until ready.",
        authors = {"Felis"}
)
public final class FelisVelocityPlugin {
    private static final Duration REGISTRATION_REFRESH = Duration.ofSeconds(15);
    private static final String SERVER_LIST_FILE = "last-servers.json"; // fallback when felis-api is down at startup
    private static final Duration WAIT_POLL = Duration.ofSeconds(2);
    private static final Duration INVITE_TTL = Duration.ofSeconds(120);
    private static final Duration INVITE_COOLDOWN = Duration.ofSeconds(30); // per sender, below the TTL
    private static final int API_THREADS = 8; // felis-api calls in flight
    private static final int API_QUEUE = 64; // calls waiting; further ones are refused
    private static final long BUSY_LOG_INTERVAL_MILLIS = 60_000L;
    private static final Duration STATS_INTERVAL = Duration.ofMinutes(10); // one health line per window
    private static final long REFRESH_OUTAGE_REPEAT_MILLIS = 5 * 60_000L; // reminder interval while refresh keeps failing

    private final ProxyServer proxy;
    private final Logger logger;
    private final Path dataDirectory;
    private final InviteBook invites = new InviteBook(INVITE_TTL.toMillis(), INVITE_COOLDOWN.toMillis());
    // Per player: burst of 5, then one command every 5 s; each acting command is a felis-api call.
    private final FrameBudget commandBudget = new FrameBudget(5, 0.2, System::currentTimeMillis);
    private final BoundedExecutor apiCalls;
    private final AtomicLong lastBusyLog = new AtomicLong();
    private final ProxyStats stats = new ProxyStats();
    private final OutageTracker refreshOutage =
            new OutageTracker(REFRESH_OUTAGE_REPEAT_MILLIS, System::currentTimeMillis);

    private FelisVelocityConfig config;
    private LinkClient linkClient;
    private FelisApiClient apiClient;
    private ServerRegistry registry;
    private ServerListSource serverList;
    private LegacyForwarding legacyForwarding;
    private WaitingRouter router;
    private boolean onlineMode;
    private boolean routingActive;

    @Inject
    public FelisVelocityPlugin(ProxyServer proxy, Logger logger, @DataDirectory Path dataDirectory) {
        this.proxy = proxy;
        this.logger = logger;
        this.dataDirectory = dataDirectory;
        this.apiCalls = new BoundedExecutor("felis-api", API_THREADS, API_QUEUE,
                t -> logger.error("Felis: a felis-api task failed", t));
    }

    @Subscribe
    public void onProxyInitialize(ProxyInitializeEvent event) {
        try {
            this.config = FelisVelocityConfig.load(dataDirectory.resolve("felis-link.properties"),
                    msg -> logger.info("Felis: {}", msg));
        } catch (Exception e) {
            logger.error("Felis disabled: {}", e.getMessage());
            return;
        }
        this.linkClient = new LinkClient(config.linkConfig());
        registerLinkCommand();
        registerFelisCommand();
        registerInviteCommand();

        warnIfBungeeChannelOpen();

        this.onlineMode = proxy.getConfiguration().isOnlineMode();
        if (!onlineMode) {
            logger.error("Felis routing DISABLED: the proxy is in offline mode (online-mode=false). "
                    + "Domain autostart and the allowlist trust Mojang-verified UUIDs; refusing to route on "
                    + "spoofable identities. /link remains available. Set online-mode=true to enable routing.");
            return;
        }
        if (!config.routingEnabled()) {
            logger.warn("Felis routing DISABLED: no root-domain configured. Add 'root-domain=' to "
                    + "felis-link.properties (or set " + FelisVelocityConfig.ENV_ROOT_DOMAIN + ") to enable "
                    + "host-based routing. /link remains available.");
            return;
        }

        this.apiClient = new FelisApiClient(config.linkConfig());
        this.registry = new ServerRegistry(proxy, logger, config.rootDomain());
        this.serverList = new ServerListSource(apiClient::listServers, dataDirectory.resolve(SERVER_LIST_FILE));
        this.legacyForwarding = new LegacyForwarding(System.getProperty(LegacyForwarding.PROPERTY),
                LegacyForwarding.detect(proxy.getClass().getClassLoader()));
        this.router = new WaitingRouter(proxy, logger, apiClient, registry, this,
                config.loginServer(), config.lobbyServer());
        MotdResponder motd = new MotdResponder(registry);
        proxy.getEventManager().register(this, router);
        proxy.getEventManager().register(this, motd);

        ControlChannel control = new ControlChannel(proxy, logger, apiClient, router, registry, this,
                config.loginServer(), config.lobbyServer());
        control.register();
        proxy.getEventManager().register(this, control);

        refreshRegistrations();
        repeating(REGISTRATION_REFRESH, this::refreshRegistrations);
        repeating(WAIT_POLL, router::tick);
        repeating(STATS_INTERVAL, this::logStats);
        // Velocity accepts no connection until this handler returns, so the first refresh above is already in.
        legacyForwarding.accepting();

        if (yieldServerCommand(proxy.getCommandManager(), proxy.getPluginManager())) {
            logger.info("Felis: Velocity's /server removed; /server now reaches the backend "
                    + "(the lobby's menu, or a server's own command).");
        }

        this.routingActive = true;
        logger.info("Felis routing ready: rootDomain={}, login={}, lobby={}. /link, /felis and /invite registered.",
                config.rootDomain(), config.loginServer(), config.lobbyServer());
    }

    /** Unregisters Velocity's built-in /server (it shadows the backend's) unless another plugin owns it. */
    static boolean yieldServerCommand(CommandManager commands, PluginManager plugins) {
        CommandMeta meta = commands.getCommandMeta("server");
        if (meta == null) {
            return false;
        }
        // built-ins belong to the "velocity" virtual plugin, or to no plugin on older proxies
        Object owner = meta.getPlugin();
        boolean builtin = owner == null || plugins.fromInstance(owner)
                .map(c -> "velocity".equals(c.getDescription().getId()))
                .orElse(false);
        if (!builtin) {
            return false;
        }
        commands.unregister("server");
        return true;
    }

    @Subscribe
    public void onDisconnect(DisconnectEvent event) {
        commandBudget.forget(event.getPlayer().getUniqueId());
    }

    @Subscribe
    public void onProxyShutdown(ProxyShutdownEvent event) {
        apiCalls.shutdown();
    }

    private boolean withinBudget(Player player) {
        if (commandBudget.tryTake(player.getUniqueId())) {
            return true;
        }
        player.sendMessage(Component.text(
                zh(player) ? "操作太频繁了，请过几秒再试。" : "Slow down — try again in a few seconds.",
                NamedTextColor.YELLOW));
        return false;
    }

    /** Runs a felis-api task off the proxy thread; returns false without running it when the pool and queue are full. */
    boolean async(Runnable task) {
        if (apiCalls.submit(task)) {
            return true;
        }
        stats.count(ProxyStats.Event.POOL_REFUSED);
        long now = System.currentTimeMillis();
        long last = lastBusyLog.get();
        if (now - last >= BUSY_LOG_INTERVAL_MILLIS && lastBusyLog.compareAndSet(last, now)) {
            logger.warn("Felis: felis-api is not keeping up ({} calls running, {} waiting); refusing new calls "
                    + "until the queue drains.", apiCalls.running(), apiCalls.waiting());
        }
        return false;
    }

    ProxyStats stats() {
        return stats;
    }

    private void logStats() {
        String line = ProxyStats.line(STATS_INTERVAL.toMinutes(), apiClient.stats().window(), stats.window(),
                router.waitingCount());
        if (line != null) {
            logger.info(line);
        }
    }

    /** Like {@link #async(Runnable)}, telling the player at once when refused. */
    boolean async(Player player, Runnable task) {
        if (async(task)) {
            return true;
        }
        player.sendMessage(Component.text(
                zh(player) ? "服务器现在很忙，请过一会儿再试。"
                           : "The network is busy right now — try again in a moment.",
                NamedTextColor.YELLOW));
        return false;
    }

    private void repeating(Duration interval, Runnable task) {
        proxy.getScheduler().buildTask(this, new SkipIfRunning(task)).delay(interval).repeat(interval).schedule();
    }

    /** Warns when {@code bungeecord:main} is open: any user backend could then kick or move any player. */
    private void warnIfBungeeChannelOpen() {
        Object cfg = proxy.getConfiguration();
        Boolean open;
        try {
            open = (Boolean) cfg.getClass().getMethod("isBungeePluginChannelEnabled").invoke(cfg);
        } catch (ReflectiveOperationException | ClassCastException | SecurityException e) {
            open = null;
        }
        if (Boolean.FALSE.equals(open)) {
            return;
        }
        logger.error("Felis: the BungeeCord plugin-message channel is {}. Every user backend can then kick or "
                        + "move any player on the network. Set 'bungee-plugin-message-channel = false' under "
                        + "[advanced] in velocity.toml (re-running the installer writes it) and restart.",
                open == null ? "in an unknown state" : "ENABLED");
    }

    private void refreshRegistrations() {
        if (router != null) {
            router.pruneLinks();
            try {
                router.setEntryPolicy(apiClient.entryPolicy());
            } catch (LinkException error) {
                logger.warn("Felis: entry policy refresh failed; retaining the last policy: {}", error.getMessage());
            }
        }
        ServerListSource.Result r = serverList.next();
        if (r.servers != null) {
            // forwarding mode first: a new legacy backend must never be reachable under modern forwarding
            LegacyForwarding.Update lf = legacyForwarding.apply(r.servers);
            if (lf != null && lf.warning) {
                logger.warn(lf.message);
            } else if (lf != null) {
                logger.info(lf.message);
            }
            registry.refresh(r.servers);
        }
        if (r.failure == null) {
            if (refreshOutage.success() == OutageTracker.Report.RECOVERED) {
                logger.info("Felis: server list refresh recovered after {} failed attempts over {} s.",
                        refreshOutage.lastOutageFailures(), refreshOutage.lastOutageMillis() / 1000);
            }
        } else {
            stats.count(ProxyStats.Event.REFRESH_FAILED);
            OutageTracker.Report report = refreshOutage.failure();
            if (r.restored) {
                logger.warn("Felis: felis-api is unreachable at startup (status={}): {}; routing to the {} backends "
                                + "in the saved server list until it answers.",
                        r.failure.statusCode(), r.failure.getMessage(), r.servers.size());
            } else if (report == OutageTracker.Report.DOWN) {
                // a transient failure must not deregister live backends
                logger.warn("Felis: server list refresh failed (status={}): {}; keeping current registrations. "
                                + "Repeats are summarized every {} min until it recovers.",
                        r.failure.statusCode(), r.failure.getMessage(), REFRESH_OUTAGE_REPEAT_MILLIS / 60_000L);
            } else if (report == OutageTracker.Report.STILL_DOWN) {
                logger.warn("Felis: server list refresh still failing: {} failed attempts over {} s, last (status={}): {}; "
                                + "keeping current registrations.",
                        refreshOutage.failures(), refreshOutage.downForMillis() / 1000,
                        r.failure.statusCode(), r.failure.getMessage());
            }
        }
        if (r.fileError != null) {
            logger.warn("Felis: saved server list {}: {}", r.failure == null ? "not written" : "not usable",
                    r.fileError.toString());
        }
    }

    private void registerLinkCommand() {
        CommandManager commands = proxy.getCommandManager();
        LiteralCommandNode<CommandSource> node = BrigadierCommand.literalArgumentBuilder("link")
                .executes(ctx -> {
                    CommandSource source = ctx.getSource();
                    if (!(source instanceof Player)) {
                        source.sendMessage(Component.text("/link can only be run by a player.", NamedTextColor.RED));
                        return Command.SINGLE_SUCCESS;
                    }
                    requestAndReply((Player) source);
                    return Command.SINGLE_SUCCESS;
                })
                .build();
        CommandMeta meta = commands.metaBuilder("link").plugin(this).build();
        commands.register(meta, new BrigadierCommand(node));
    }

    private void requestAndReply(Player player) {
        if (!withinBudget(player)) {
            return;
        }
        boolean zh = zh(player);
        player.sendMessage(Component.text(
                zh ? "正在获取绑定码……" : "Requesting a link code…", NamedTextColor.GRAY));
        async(player, () -> {
            try {
                LinkCode code = linkClient.requestCode(player.getUniqueId());
                for (Component line : LinkCodeCard.lines(code.code(), code.panelUrl(), zh)) {
                    player.sendMessage(line);
                }
            } catch (LinkException e) {
                logger.warn("link code request failed for {} (status={}, code={}): {}",
                        player.getUniqueId(), e.statusCode(), e.errorCode(), e.getMessage());
                player.sendMessage(Component.text(
                        zh ? "现在无法获取绑定码，请稍后再试。"
                           : "Couldn't get a link code right now. Please try again in a moment.",
                        NamedTextColor.RED));
            }
        });
    }

    /** Url-safe charset for an op-login code, so path-dangerous input is refused before it enters a request path. */
    private static final Pattern OP_LOGIN_CODE = Pattern.compile("^[A-Za-z0-9_-]{1,128}$");

    private void registerFelisCommand() {
        CommandManager commands = proxy.getCommandManager();
        LiteralCommandNode<CommandSource> node = BrigadierCommand.literalArgumentBuilder("felis")
                .executes(ctx -> {
                    sendSummary(ctx.getSource());
                    return Command.SINGLE_SUCCESS;
                })
                .then(BrigadierCommand.literalArgumentBuilder("help")
                        .executes(ctx -> {
                            sendHelp(ctx.getSource());
                            return Command.SINGLE_SUCCESS;
                        }))
                .then(BrigadierCommand.literalArgumentBuilder("server")
                        .executes(ctx -> {
                            sendServerList(ctx.getSource());
                            return Command.SINGLE_SUCCESS;
                        }))
                .then(BrigadierCommand.literalArgumentBuilder("lobby")
                        .executes(ctx -> {
                            doLobby(ctx.getSource());
                            return Command.SINGLE_SUCCESS;
                        }))
                .then(BrigadierCommand.literalArgumentBuilder("go")
                        .then(BrigadierCommand.requiredArgumentBuilder("server", StringArgumentType.word())
                                .executes(ctx -> {
                                    doGo(ctx.getSource(), StringArgumentType.getString(ctx, "server"), false);
                                    return Command.SINGLE_SUCCESS;
                                })))
                .then(BrigadierCommand.literalArgumentBuilder("claim")
                        .executes(ctx -> {
                            doClaim(ctx.getSource());
                            return Command.SINGLE_SUCCESS;
                        }))
                .then(BrigadierCommand.literalArgumentBuilder("migrate")
                        .executes(ctx -> {
                            doMigrate(ctx.getSource());
                            return Command.SINGLE_SUCCESS;
                        }))
                .then(BrigadierCommand.literalArgumentBuilder("web")
                        .executes(ctx -> {
                            sendWebInfo(ctx.getSource());
                            return Command.SINGLE_SUCCESS;
                        })
                        .then(BrigadierCommand.literalArgumentBuilder("op")
                                .executes(ctx -> {
                                    sendWebOpInfo(ctx.getSource());
                                    return Command.SINGLE_SUCCESS;
                                })
                                .then(BrigadierCommand.literalArgumentBuilder("approve")
                                        .then(BrigadierCommand.requiredArgumentBuilder("code", StringArgumentType.word())
                                                .executes(ctx -> {
                                                    doOpApprove(ctx.getSource(), StringArgumentType.getString(ctx, "code"), null);
                                                    return Command.SINGLE_SUCCESS;
                                                })
                                                .then(BrigadierCommand.requiredArgumentBuilder("account", StringArgumentType.word())
                                                        .executes(ctx -> {
                                                            doOpApprove(ctx.getSource(),
                                                                    StringArgumentType.getString(ctx, "code"),
                                                                    StringArgumentType.getString(ctx, "account"));
                                                            return Command.SINGLE_SUCCESS;
                                                        }))))))
                .build();
        CommandMeta meta = commands.metaBuilder("felis").plugin(this).build();
        commands.register(meta, new BrigadierCommand(node));
    }

    private Player requirePlayer(CommandSource source) {
        if (source instanceof Player) {
            return (Player) source;
        }
        source.sendMessage(Component.text("That command can only be run in-game by a player.", NamedTextColor.RED));
        return null;
    }

    // Fails closed: a player on no backend yet, or still on the login server, is refused.
    private boolean ensureOutOfLimbo(Player player) {
        boolean zh = zh(player);
        Optional<ServerConnection> current = player.getCurrentServer();
        if (current.isEmpty()) {
            player.sendMessage(Component.text(
                    zh ? "请稍候——完成连接后再使用 /felis。"
                       : "Hold on — finish connecting before using /felis.", NamedTextColor.YELLOW));
            return false;
        }
        if (config.loginServer().equalsIgnoreCase(current.get().getServerInfo().getName())) {
            player.sendMessage(Component.text(
                    zh ? "请先完成登录——登录区内无法使用 /felis。"
                       : "Finish signing in first — /felis isn't available from the login area.",
                    NamedTextColor.YELLOW));
            return false;
        }
        return true;
    }

    private boolean gateInfo(CommandSource source) {
        return !(source instanceof Player) || ensureOutOfLimbo((Player) source);
    }

    private void sendSummary(CommandSource source) {
        if (!gateInfo(source)) {
            return;
        }
        boolean zh = zh(source);
        source.sendMessage(Component.text("Felis proxy", NamedTextColor.AQUA));
        source.sendMessage(field("online-mode", String.valueOf(onlineMode)));
        if (!routingActive) {
            source.sendMessage(Component.text(
                    zh ? "  routing: 已禁用" + (onlineMode ? "（未设置 root-domain）" : "（离线模式）")
                       : "  routing: disabled" + (onlineMode ? " (no root-domain set)" : " (offline mode)"),
                    NamedTextColor.YELLOW));
            source.sendMessage(Component.text(
                    zh ? "  /felis help 查看命令" : "  /felis help for commands", NamedTextColor.GRAY));
            return;
        }
        source.sendMessage(field("root-domain", config.rootDomain()));
        source.sendMessage(field("login", config.loginServer()));
        source.sendMessage(field("lobby", config.lobbyServer()));
        source.sendMessage(field("servers", String.valueOf(registry.all().size())));
        if (!(source instanceof Player)) {
            source.sendMessage(field("felis-api", apiClient.stats().total().summary()));
            source.sendMessage(field("waiting", String.valueOf(router.waitingCount())));
            StringBuilder failures = new StringBuilder();
            for (ProxyStats.Event e : ProxyStats.Event.values()) {
                failures.append(failures.length() == 0 ? "" : ", ").append(e.label).append('=').append(stats.total(e));
            }
            source.sendMessage(field("since start", failures.toString()));
        }
        source.sendMessage(Component.text(
                zh ? "  /felis help 查看命令" : "  /felis help for commands", NamedTextColor.GRAY));
    }

    private void sendHelp(CommandSource source) {
        boolean zh = zh(source);
        source.sendMessage(Component.text(zh ? "Felis 命令" : "Felis commands", NamedTextColor.AQUA));
        helpLine(source, "/felis",
                zh ? "代理与路由状态" : "proxy and routing status");
        helpLine(source, "/felis server",
                zh ? "此代理已知的 felis 服务器" : "the felis servers this proxy knows");
        helpLine(source, "/felis go <server>",
                zh ? "启动服务器并在就绪后把你传送过去" : "start a server and move you in when it's ready");
        helpLine(source, "/felis lobby",
                zh ? "回到大厅" : "back to the lobby");
        helpLine(source, "/felis claim",
                zh ? "认领你所在的服务器" : "take ownership of the server you're on");
        helpLine(source, "/felis migrate",
                zh ? "把你的服务器迁移到另一个账户" : "move your servers to another account");
        helpLine(source, "/felis web",
                zh ? "网页控制台地址" : "where the web consoles live");
        helpLine(source, "/felis web op approve <code>",
                zh ? "查看并批准待处理的管理员登录" : "review and approve a pending operator sign-in");
    }

    private void sendServerList(CommandSource source) {
        if (!gateInfo(source)) {
            return;
        }
        boolean zh = zh(source);
        if (!routingActive) {
            source.sendMessage(Component.text(
                    zh ? "Felis 路由已禁用。" : "Felis routing is disabled.", NamedTextColor.YELLOW));
            return;
        }
        List<ServerView> servers = registry.all().stream()
                .filter(v -> !isSystemServer(v.name()))
                .toList();
        if (servers.isEmpty()) {
            source.sendMessage(Component.text(
                    zh ? "暂无已知的 felis 服务器。" : "No felis servers known yet.", NamedTextColor.GRAY));
            return;
        }
        source.sendMessage(Component.text(zh ? "Felis 服务器：" : "Felis servers:", NamedTextColor.AQUA));
        for (ServerView v : servers) {
            String phase = v.phase() == null ? "?" : v.phase();
            String ready = v.ready() ? (zh ? "，就绪" : ", ready") : "";
            source.sendMessage(Component.text("  " + v.name() + " ", NamedTextColor.WHITE)
                    .append(Component.text("[" + phase + ready + "]",
                            v.ready() ? NamedTextColor.GREEN : NamedTextColor.GRAY)));
        }
    }

    /** Returns false when a guard refused (and told the player); {@code joinIfReady} is set only by an accepted invite. */
    private boolean doGo(CommandSource source, String serverArg, boolean joinIfReady) {
        Player player = requirePlayer(source);
        if (player == null || !ensureOutOfLimbo(player)) {
            return false;
        }
        boolean zh = zh(player);
        if (!routingActive) {
            player.sendMessage(routingDisabled(zh));
            return false;
        }
        String target = serverArg.trim();
        if (target.equalsIgnoreCase(config.lobbyServer())) {
            // the lobby is a system server, so the loop below would not find it
            router.toLobby(player);
            return true;
        }
        ServerView match = null;
        for (ServerView v : registry.all()) {
            if (!isSystemServer(v.name()) && v.name().equalsIgnoreCase(target)) {
                match = v;
                break;
            }
        }
        if (match == null) {
            player.sendMessage(Component.text(
                    zh ? "没有名为「" + target + "」的 felis 服务器。试试 /felis server。"
                       : "No felis server named « " + target + " ». Try /felis server.", NamedTextColor.YELLOW));
            return false;
        }
        Optional<ServerConnection> current = player.getCurrentServer();
        if (current.isPresent() && current.get().getServerInfo().getName().equalsIgnoreCase(match.name())) {
            player.sendMessage(Component.text(
                    zh ? "你已经在「" + match.name() + "」上了。"
                       : "You're already on « " + match.name() + " ».", NamedTextColor.GRAY));
            return false;
        }
        if (!withinBudget(player)) {
            return false;
        }
        if (joinIfReady) {
            router.enqueueFromInvite(player, match.name());
        } else {
            router.enqueueFromCommand(player, match.name());
        }
        return true;
    }

    private void doLobby(CommandSource source) {
        Player player = requirePlayer(source);
        if (player == null || !ensureOutOfLimbo(player)) {
            return;
        }
        if (!routingActive) {
            player.sendMessage(routingDisabled(zh(player)));
            return;
        }
        router.toLobby(player);
    }

    private void doClaim(CommandSource source) {
        Player player = requirePlayer(source);
        if (player == null || !ensureOutOfLimbo(player)) {
            return;
        }
        boolean zh = zh(player);
        if (!routingActive) {
            player.sendMessage(routingDisabled(zh));
            return;
        }
        Optional<ServerConnection> current = player.getCurrentServer();
        if (current.isEmpty()) {
            player.sendMessage(Component.text(
                    zh ? "请先加入一个服务器再认领。" : "Join a server before claiming it.",
                    NamedTextColor.YELLOW));
            return;
        }
        String name = current.get().getServerInfo().getName();
        if (!registry.isManaged(name)) {
            player.sendMessage(Component.text(
                    zh ? "「" + name + "」不是可认领的 felis 服务器。"
                       : "« " + name + " » isn't a claimable felis server.", NamedTextColor.YELLOW));
            return;
        }
        if (!withinBudget(player)) {
            return;
        }
        UUID uuid = player.getUniqueId();
        player.sendMessage(Component.text(
                zh ? "正在认领「" + name + "」……" : "Claiming « " + name + " »…", NamedTextColor.GRAY));
        async(player, () -> {
            try {
                apiClient.claim(name, uuid);
                player.sendMessage(Component.text(
                        zh ? "你现在拥有「" + name + "」了。" : "You now own « " + name + " ».",
                        NamedTextColor.GREEN));
            } catch (LinkException e) {
                player.sendMessage(Component.text(claimError(e, name, zh), NamedTextColor.RED));
            }
        });
    }

    // Opens the migration only; the player finishes it on the web console.
    private void doMigrate(CommandSource source) {
        Player player = requirePlayer(source);
        if (player == null || !ensureOutOfLimbo(player)) {
            return;
        }
        boolean zh = zh(player);
        if (!routingActive) {
            player.sendMessage(routingDisabled(zh));
            return;
        }
        if (!withinBudget(player)) {
            return;
        }
        UUID uuid = player.getUniqueId();
        String who = player.getUsername();
        player.sendMessage(Component.text(
                zh ? "正在发起账户迁移……" : "Starting account migration…", NamedTextColor.GRAY));
        async(player, () -> {
            try {
                apiClient.migrateStart(uuid);
                String panelHost = config.panelHostname();
                player.sendMessage(Component.text(
                        zh ? "迁移已发起——请在网页控制台完成："
                           : "Migration started — finish it on the web console:", NamedTextColor.GREEN));
                player.sendMessage(Component.text(
                        "  " + (panelHost == null
                                ? (zh ? "玩家网页控制台" : "the players' web console")
                                : "https://" + panelHost + "/account"),
                        NamedTextColor.WHITE));
                player.sendMessage(Component.text(
                        zh ? "你需要确认身份、指定接收服务器的账户，然后获得一个迁移码。"
                           : "You'll confirm it's you, name the account to receive your servers, then get a code.",
                        NamedTextColor.GRAY));
                logger.info("Felis: account migration started in-game by {} ({})", who, uuid);
            } catch (LinkException e) {
                player.sendMessage(Component.text(migrateError(e, zh), NamedTextColor.RED));
            }
        });
    }

    private void sendWebInfo(CommandSource source) {
        if (!gateInfo(source)) {
            return;
        }
        boolean zh = zh(source);
        String panelHost = config.panelHostname();
        String adminHost = config.adminHostname();
        if (panelHost == null && adminHost == null) {
            source.sendMessage(Component.text(
                    zh ? "此代理未配置网页控制台。"
                       : "The web console isn't configured on this proxy.", NamedTextColor.YELLOW));
            return;
        }
        source.sendMessage(Component.text(
                zh ? "Felis 网页控制台" : "Felis web consoles", NamedTextColor.AQUA));
        if (panelHost != null) {
            source.sendMessage(field(zh ? "玩家" : "players", "https://" + panelHost));
        }
        if (adminHost != null) {
            source.sendMessage(field(zh ? "管理员" : "operators", "https://" + adminHost));
        }
        source.sendMessage(Component.text(
                zh ? "  管理员：/felis web op approve <code> 查看并批准待处理的登录"
                   : "  operators: /felis web op approve <code> reviews a pending sign-in",
                NamedTextColor.GRAY));
    }

    private void sendWebOpInfo(CommandSource source) {
        if (!gateInfo(source)) {
            return;
        }
        boolean zh = zh(source);
        String adminHost = config.adminHostname();
        String site = adminHost != null ? adminHost : (zh ? "管理员控制台" : "the operator console");
        source.sendMessage(Component.text(
                zh ? "管理员登录" : "Operator sign-in", NamedTextColor.AQUA));
        source.sendMessage(Component.text(
                zh ? "管理员在 " + site + " 登录时会显示一个批准码。运行"
                   : "An operator signing in at " + site + " shows an approval code. Run",
                NamedTextColor.GRAY));
        source.sendMessage(Component.text("  /felis web op approve <code>", NamedTextColor.WHITE));
        source.sendMessage(Component.text(
                zh ? "查看这是谁的登录，确认后输入其账户名即可担保——你必须是已绑定并在线的管理员。"
                   : "to see whose sign-in it is, then confirm with their account name to vouch for it"
                     + " — you must be an online, linked administrator.",
                NamedTextColor.GRAY));
    }

    // With no account name this only shows whose sign-in the code is (OpApprovalCard); with one it approves,
    // and felis-api refuses unless the name is that request's account.
    private void doOpApprove(CommandSource source, String codeArg, String accountArg) {
        Player player = requirePlayer(source);
        if (player == null || !ensureOutOfLimbo(player)) {
            return;
        }
        boolean zh = zh(player);
        if (!routingActive) {
            player.sendMessage(routingDisabled(zh));
            return;
        }
        String code = codeArg.trim();
        if (!OP_LOGIN_CODE.matcher(code).matches()) {
            player.sendMessage(Component.text(
                    zh ? "这不像一个有效的批准码。" : "That doesn't look like a valid approval code.",
                    NamedTextColor.RED));
            return;
        }
        if (!withinBudget(player)) {
            return;
        }
        UUID approver = player.getUniqueId();
        String who = player.getUsername();
        if (accountArg == null) {
            async(player, () -> {
                try {
                    OpLoginView req = apiClient.opLoginShow(code, approver);
                    long age = req.createdAt() == null ? -1
                            : Math.max(0, Duration.between(req.createdAt(), Instant.now()).getSeconds());
                    for (Component line : OpApprovalCard.lines(req, code, zh, age)) {
                        player.sendMessage(line);
                    }
                } catch (LinkException e) {
                    player.sendMessage(Component.text(opApproveError(e, code, zh), NamedTextColor.RED));
                }
            });
            return;
        }
        String account = accountArg.trim();
        player.sendMessage(Component.text(
                zh ? "正在批准管理员登录……" : "Approving operator sign-in…", NamedTextColor.GRAY));
        async(player, () -> {
            try {
                OpLoginView done = apiClient.opLoginApprove(code, approver, account);
                player.sendMessage(Component.text(
                        zh ? "已批准 " + done.username() + "（" + done.email() + "）的登录——对方现在可以完成登录了。"
                           : "Approved " + done.username() + " (" + done.email()
                             + ") — they can finish signing in now.", NamedTextColor.GREEN));
                logger.info("Felis: op-login {} for {} approved in-game by {} ({})",
                        code, done.username(), who, approver);
            } catch (LinkException e) {
                player.sendMessage(Component.text(opApproveError(e, code, zh), NamedTextColor.RED));
            }
        });
    }

    // /invite wraps /felis go, run as the accepting player, so it grants no access the invitee lacked.
    // A stored invite carries only a server name, never an identity to act as.

    private void registerInviteCommand() {
        CommandManager commands = proxy.getCommandManager();
        LiteralCommandNode<CommandSource> node = BrigadierCommand.literalArgumentBuilder("invite")
                .executes(ctx -> {
                    sendInviteUsage(ctx.getSource());
                    return Command.SINGLE_SUCCESS;
                })
                // the optional <server> pins a button click to the invite that drew it
                .then(BrigadierCommand.literalArgumentBuilder("accept")
                        .executes(ctx -> {
                            doInviteAnswer(ctx.getSource(), true, null);
                            return Command.SINGLE_SUCCESS;
                        })
                        .then(BrigadierCommand.requiredArgumentBuilder("server", StringArgumentType.word())
                                .executes(ctx -> {
                                    doInviteAnswer(ctx.getSource(), true,
                                            StringArgumentType.getString(ctx, "server"));
                                    return Command.SINGLE_SUCCESS;
                                })))
                .then(BrigadierCommand.literalArgumentBuilder("deny")
                        .executes(ctx -> {
                            doInviteAnswer(ctx.getSource(), false, null);
                            return Command.SINGLE_SUCCESS;
                        })
                        .then(BrigadierCommand.requiredArgumentBuilder("server", StringArgumentType.word())
                                .executes(ctx -> {
                                    doInviteAnswer(ctx.getSource(), false,
                                            StringArgumentType.getString(ctx, "server"));
                                    return Command.SINGLE_SUCCESS;
                                })))
                // literals match before arguments, so a player named "accept" or "deny" cannot be invited by name
                .then(BrigadierCommand.requiredArgumentBuilder("player", StringArgumentType.word())
                        .suggests((ctx, builder) -> {
                            String typed = builder.getRemaining().toLowerCase(Locale.ROOT);
                            UUID self = ctx.getSource() instanceof Player
                                    ? ((Player) ctx.getSource()).getUniqueId() : null;
                            proxy.getAllPlayers().stream()
                                    .filter(p -> !p.getUniqueId().equals(self))
                                    .map(Player::getUsername)
                                    .filter(name -> name.toLowerCase(Locale.ROOT).startsWith(typed))
                                    .forEach(builder::suggest);
                            return builder.buildFuture();
                        })
                        .executes(ctx -> {
                            doInvite(ctx.getSource(), StringArgumentType.getString(ctx, "player"));
                            return Command.SINGLE_SUCCESS;
                        }))
                .build();
        CommandMeta meta = commands.metaBuilder("invite").plugin(this).build();
        commands.register(meta, new BrigadierCommand(node));
    }

    private void sendInviteUsage(CommandSource source) {
        boolean zh = zh(source);
        source.sendMessage(Component.text(zh ? "邀请玩家" : "Invite a player", NamedTextColor.AQUA));
        helpLine(source, "/invite <player>",
                zh ? "邀请一名在线玩家来你所在的服务器" : "invite an online player to the server you're on");
        helpLine(source, "/invite accept",
                zh ? "接受待处理的邀请" : "accept your pending invite");
        helpLine(source, "/invite deny",
                zh ? "拒绝待处理的邀请" : "decline your pending invite");
    }

    private void doInvite(CommandSource source, String playerArg) {
        Player inviter = requirePlayer(source);
        if (inviter == null || !ensureOutOfLimbo(inviter)) {
            return;
        }
        boolean zh = zh(inviter);
        if (!routingActive) {
            inviter.sendMessage(routingDisabled(zh));
            return;
        }
        Optional<ServerConnection> current = inviter.getCurrentServer();
        if (current.isEmpty()
                || isSystemServer(current.get().getServerInfo().getName())
                || !registry.isManaged(current.get().getServerInfo().getName())) {
            inviter.sendMessage(Component.text(
                    zh ? "只能邀请别人来你所在的 felis 服务器——你现在不在这样的服务器上。"
                       : "You can only invite someone to a felis server you're on — you aren't on one.",
                    NamedTextColor.YELLOW));
            return;
        }
        String server = current.get().getServerInfo().getName();

        String target = playerArg.trim();
        Optional<Player> found = proxy.getPlayer(target);
        if (found.isEmpty()) {
            inviter.sendMessage(Component.text(
                    zh ? "「" + target + "」不在线。" : "« " + target + " » isn't online.",
                    NamedTextColor.YELLOW));
            return;
        }
        Player invitee = found.get();
        if (invitee.getUniqueId().equals(inviter.getUniqueId())) {
            inviter.sendMessage(Component.text(
                    zh ? "你不用邀请自己。" : "You don't need to invite yourself.", NamedTextColor.GRAY));
            return;
        }
        // an invitee at the login gate could not answer the card (doInviteAnswer refuses in limbo)
        Optional<ServerConnection> theirs = invitee.getCurrentServer();
        if (theirs.isEmpty()
                || config.loginServer().equalsIgnoreCase(theirs.get().getServerInfo().getName())) {
            inviter.sendMessage(Component.text(
                    zh ? "「" + invitee.getUsername() + "」还没完成登录，现在收不了邀请。"
                       : "« " + invitee.getUsername() + " » hasn't finished signing in yet.",
                    NamedTextColor.YELLOW));
            return;
        }
        if (theirs.get().getServerInfo().getName().equalsIgnoreCase(server)) {
            inviter.sendMessage(Component.text(
                    zh ? "「" + invitee.getUsername() + "」已经在「" + server + "」上了。"
                       : "« " + invitee.getUsername() + " » is already on « " + server + " ».",
                    NamedTextColor.GRAY));
            return;
        }

        // last gate, so refused invites do not use up the cooldown
        long now = System.currentTimeMillis();
        long wait = invites.cooldownRemaining(inviter.getUniqueId(), now);
        if (wait > 0) {
            long secs = (wait + 999) / 1000; // round up
            inviter.sendMessage(Component.text(
                    zh ? "邀请发得太快了，请 " + secs + " 秒后再试。"
                       : "Too many invites — try again in " + secs + "s.",
                    NamedTextColor.YELLOW));
            return;
        }

        invites.put(invitee.getUniqueId(), inviter.getUniqueId(), server, now);
        sendInviteCard(invitee, inviter.getUsername(), server);
        inviter.sendMessage(Component.text(
                zh ? "已邀请「" + invitee.getUsername() + "」前往「" + server + "」。"
                   : "Invited « " + invitee.getUsername() + " » to « " + server + " ».",
                NamedTextColor.GREEN));
        inviter.sendMessage(accessNotice(server, zh));
    }

    /** Warns that accepting records the invitee in the allowlist, which on an {@code allowlist}-policy server lets them start it. */
    private Component accessNotice(String server, boolean zh) {
        ServerView view = registry.view(server);
        boolean gatesOnAllowlist = view != null && "allowlist".equalsIgnoreCase(view.autostartPolicy());
        return Component.text(
                gatesOnAllowlist
                        ? (zh ? "  提示：TA 接受后会被加入「" + server + "」的白名单，之后可以自行进入并启动这台服务器。"
                              : "  Note: accepting adds them to « " + server + " »'s allowlist — they'll then be"
                                + " able to come back and start it themselves.")
                        : (zh ? "  提示：TA 接受后会被记入「" + server + "」的白名单。"
                              : "  Note: accepting records them in « " + server + " »'s allowlist."),
                NamedTextColor.GRAY);
    }

    private void sendInviteCard(Player invitee, String inviterName, String server) {
        InviteCard.lines(inviterName, server, zh(invitee), INVITE_TTL.toSeconds())
                .forEach(invitee::sendMessage);
    }

    /** {@code fromCard} is the server the clicked card named, or null when the subcommand was typed bare. */
    private void doInviteAnswer(CommandSource source, boolean accept, String fromCard) {
        Player player = requirePlayer(source);
        if (player == null || !ensureOutOfLimbo(player)) {
            return;
        }
        boolean zh = zh(player);
        long now = System.currentTimeMillis();
        InviteBook.Invite pending = invites.peek(player.getUniqueId(), now);
        if (pending == null) {
            player.sendMessage(Component.text(
                    zh ? "你没有待处理的邀请（可能已过期）。"
                       : "You have no pending invite (it may have expired).", NamedTextColor.YELLOW));
            return;
        }
        // checked before taking: a click on a superseded card must leave the live invite alone
        if (fromCard != null && !fromCard.equalsIgnoreCase(pending.server())) {
            player.sendMessage(Component.text(
                    zh ? "这张邀请卡已被新的邀请取代——你当前的邀请是前往「" + pending.server() + "」。"
                       : "That invite was superseded — your pending one is to « " + pending.server() + " ».",
                    NamedTextColor.YELLOW));
            return;
        }
        // peek-then-take is not atomic; a racing newer invite is taken instead, which matches newest-wins
        InviteBook.Invite invite = invites.take(player.getUniqueId(), now);
        if (invite == null) {
            return; // answered by a racing click; that one owns the reply
        }
        if (!accept) {
            notifyInviter(invite, player.getUsername(), Answer.DECLINED);
            player.sendMessage(Component.text(
                    zh ? "已拒绝邀请。" : "Invite declined.", NamedTextColor.GRAY));
            return;
        }
        // doGo re-runs every guard; the inviter is told after the handoff, not on the click
        notifyInviter(invite, player.getUsername(),
                doGo(player, invite.server(), true) ? Answer.ACCEPTED : Answer.FAILED);
    }

    private enum Answer { ACCEPTED, DECLINED, FAILED }

    // ACCEPTED means the transfer reached the waiting queue; a later wake failure is reported to the guest only.
    private void notifyInviter(InviteBook.Invite invite, String who, Answer answer) {
        proxy.getPlayer(invite.from()).ifPresent(p -> {
            boolean zh = zh(p);
            switch (answer) {
                case ACCEPTED -> p.sendMessage(Component.text(
                        zh ? "「" + who + "」接受了你的邀请。" : "« " + who + " » accepted your invite.",
                        NamedTextColor.GREEN));
                case DECLINED -> p.sendMessage(Component.text(
                        zh ? "「" + who + "」拒绝了你的邀请。" : "« " + who + " » declined your invite.",
                        NamedTextColor.GRAY));
                case FAILED -> p.sendMessage(Component.text(
                        zh ? "「" + who + "」接受了邀请，但没能过来。"
                           : "« " + who + " » accepted, but couldn't get through.",
                        NamedTextColor.YELLOW));
            }
        });
    }

    /** True when the source is a player whose client locale is Chinese; the console and unknown locales read English. */
    static boolean zh(CommandSource source) {
        if (!(source instanceof Player)) {
            return false;
        }
        Locale locale = ((Player) source).getPlayerSettings().getLocale();
        return locale != null && "zh".equalsIgnoreCase(locale.getLanguage());
    }

    private Component routingDisabled(boolean zh) {
        return Component.text(
                zh ? "此代理已禁用 Felis 路由" + (onlineMode ? "（未设置 root-domain）。" : "（离线模式）。")
                   : "Felis routing is disabled on this proxy" + (onlineMode ? " (no root-domain set)." : " (offline mode)."),
                NamedTextColor.YELLOW);
    }

    private boolean isSystemServer(String name) {
        return config.loginServer().equalsIgnoreCase(name)
                || config.lobbyServer().equalsIgnoreCase(name);
    }

    private static String claimError(LinkException e, String server, boolean zh) {
        switch (e.statusCode()) {
            case 412:
                return zh ? "请先在网页控制台绑定账户，再认领服务器。"
                          : "Link your account on the web console before claiming a server.";
            case 403:
                return zh ? "你已达到服务器数量上限——无法再认领。"
                          : "You've reached your server limit — you can't claim another.";
            case 409:
                return zh ? "「" + server + "」已有主人。"
                          : "« " + server + " » is already owned.";
            case 404:
                return zh ? "「" + server + "」已不可用。"
                          : "« " + server + " » is no longer available.";
            case 0:
                return zh ? "Felis 暂时不可用——请稍后再试。"
                          : "Felis is temporarily unavailable — please try again.";
            default:
                return zh ? "现在无法认领「" + server + "」。请稍后再试。"
                          : "Couldn't claim « " + server + " » right now. Please try again.";
        }
    }

    private static String migrateError(LinkException e, boolean zh) {
        switch (e.statusCode()) {
            case 404:
                return zh ? "请先在网页控制台绑定账户，再进行迁移。"
                          : "Link your account on the web console before migrating.";
            case 409:
                return zh ? "此账户无法发起迁移（已迁移或已停用）。"
                          : "This account can't start a migration (already migrated or retired).";
            case 0:
                return zh ? "Felis 暂时不可用——请稍后再试。"
                          : "Felis is temporarily unavailable — please try again.";
            default:
                return zh ? "现在无法发起迁移。请稍后再试。"
                          : "Couldn't start the migration right now. Please try again.";
        }
    }

    private static String opApproveError(LinkException e, String code, boolean zh) {
        switch (e.statusCode()) {
            case 403:
                return zh ? "只有已绑定的管理员才能批准管理员登录。"
                          : "Only a linked administrator may approve an operator sign-in.";
            case 404:
                return zh ? "没有携带该码的待处理管理员登录（可能已过期）。"
                          : "No pending operator sign-in with that code (it may have expired).";
            case 409:
                return zh ? "该登录属于另一个账户，未批准。运行 /felis web op approve " + code + " 查看是谁的登录。"
                          : "That sign-in is for a different account, so it was not approved. Run /felis web op approve "
                            + code + " to see whose it is.";
            case 0:
                return zh ? "Felis 暂时不可用——请稍后再试。"
                          : "Felis is temporarily unavailable — please try again.";
            default:
                return zh ? "现在无法批准该登录。请稍后再试。"
                          : "Couldn't approve that sign-in right now. Please try again.";
        }
    }

    private static void helpLine(CommandSource source, String cmd, String desc) {
        source.sendMessage(Component.text("  " + cmd + " ", NamedTextColor.WHITE)
                .append(Component.text("— " + desc, NamedTextColor.GRAY)));
    }

    private static Component field(String key, String value) {
        return Component.text("  " + key + ": ", NamedTextColor.GRAY)
                .append(Component.text(value == null ? "<unset>" : value, NamedTextColor.WHITE));
    }
}
