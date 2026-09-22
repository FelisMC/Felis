package best.lolicon.felis.velocity;

import best.lolicon.felis.link.FelisApiClient;
import best.lolicon.felis.link.LinkClient;
import best.lolicon.felis.link.LinkCode;
import best.lolicon.felis.link.LinkException;
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
import com.velocitypowered.api.event.proxy.ProxyInitializeEvent;
import com.velocitypowered.api.plugin.Plugin;
import com.velocitypowered.api.plugin.annotation.DataDirectory;
import com.velocitypowered.api.proxy.Player;
import com.velocitypowered.api.proxy.ProxyServer;
import com.velocitypowered.api.proxy.ServerConnection;
import net.kyori.adventure.text.Component;
import net.kyori.adventure.text.format.NamedTextColor;
import org.slf4j.Logger;

import java.nio.file.Path;
import java.time.Duration;
import java.util.List;
import java.util.Locale;
import java.util.Optional;
import java.util.UUID;
import java.util.regex.Pattern;

/**
 * FelisVelocityPlugin is the proxy-side of Felis (spec §10 account-link + §11
 * domain-autostart routing). Velocity sits on the player-facing edge, off-cluster,
 * and is where two responsibilities naturally live:
 *
 * <ul>
 *   <li><b>{@code /link}</b> — mints a one-time account-link code from the player's
 *       online-mode-verified UUID (the first leg of §10), unchanged.</li>
 *   <li><b>Domain-autostart routing</b> — recognizes each server's subdomain,
 *       registers backends dynamically, routes joins, wakes a sleeping target and
 *       holds the player in a lobby until it is ready, and reports real joins so
 *       the reaper and allowlist see them (§11, driving §9).</li>
 * </ul>
 *
 * <p>Routing has two hard preconditions, each fails safe: the proxy must run in
 * online mode (verified UUIDs are the whole basis of the autostartPolicy and
 * allowlist gates — under offline mode routing is refused while {@code /link}
 * keeps working), and a {@code root-domain} must be configured (the only place the
 * deployment zone enters the proxy; never compiled in). With routing active the
 * {@code felis:control} plugin-message channel (spec §12) is also opened: the
 * felis-paper lobby's {@code /menu} drives the same waiting queue through
 * {@link ControlChannel}, deriving identity from the backend connection rather than
 * the frame so a compromised lobby cannot act as another player (spec §14).
 */
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
    private static final Duration WAIT_POLL = Duration.ofSeconds(2);
    private static final Duration INVITE_TTL = Duration.ofSeconds(120);
    // Long enough that spraying cards at a room is tedious, short enough that showing three
    // friends around one after another is not. Sub-TTL on purpose: a sender may hold several
    // live invites, they just cannot post them all in one breath.
    private static final Duration INVITE_COOLDOWN = Duration.ofSeconds(30);

    private final ProxyServer proxy;
    private final Logger logger;
    private final Path dataDirectory;
    private final InviteBook invites = new InviteBook(INVITE_TTL.toMillis(), INVITE_COOLDOWN.toMillis());

    private FelisVelocityConfig config;
    private LinkClient linkClient;
    private FelisApiClient apiClient;
    private ServerRegistry registry;
    private WaitingRouter router;
    private boolean onlineMode;
    private boolean routingActive;

    @Inject
    public FelisVelocityPlugin(ProxyServer proxy, Logger logger, @DataDirectory Path dataDirectory) {
        this.proxy = proxy;
        this.logger = logger;
        this.dataDirectory = dataDirectory;
    }

    @Subscribe
    public void onProxyInitialize(ProxyInitializeEvent event) {
        try {
            this.config = FelisVelocityConfig.load(dataDirectory.resolve("felis-link.properties"));
        } catch (Exception e) {
            logger.error("Felis disabled: {}", e.getMessage());
            return;
        }
        this.linkClient = new LinkClient(config.linkConfig());
        registerLinkCommand();
        registerFelisCommand();
        registerInviteCommand();

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
        this.router = new WaitingRouter(proxy, logger, apiClient, registry, this,
                config.loginServer(), config.lobbyServer());
        MotdResponder motd = new MotdResponder(registry);
        proxy.getEventManager().register(this, router);
        proxy.getEventManager().register(this, motd);

        // The felis:control face (spec §12): the felis-paper lobby's /menu drives the
        // same waiting queue through this channel. Opened only with routing active —
        // it depends on the same online-mode + root-domain guards, and its claim/wake
        // identity is the verified UUID off the backend connection (spec §14).
        ControlChannel control = new ControlChannel(proxy, logger, apiClient, router, this);
        control.register();
        proxy.getEventManager().register(this, control);

        // Prime registrations immediately, then keep them fresh; drain the queue often.
        refreshRegistrations();
        repeating(REGISTRATION_REFRESH, this::refreshRegistrations);
        repeating(WAIT_POLL, router::tick);

        this.routingActive = true;
        logger.info("Felis routing ready: rootDomain={}, login={}, lobby={}. /link, /felis and /invite registered.",
                config.rootDomain(), config.loginServer(), config.lobbyServer());
    }

    /** async runs a task on Velocity's scheduler so felis-api I/O never blocks the proxy thread. */
    void async(Runnable task) {
        proxy.getScheduler().buildTask(this, task).schedule();
    }

    private void repeating(Duration interval, Runnable task) {
        proxy.getScheduler().buildTask(this, task).delay(interval).repeat(interval).schedule();
    }

    private void refreshRegistrations() {
        try {
            List<ServerView> servers = apiClient.listServers();
            registry.refresh(servers);
        } catch (LinkException e) {
            // Keep existing registrations on a control-plane blip (spec §11): a
            // transient failure must never deregister live backends.
            logger.warn("Felis: server list refresh failed (status={}): {}; keeping current registrations.",
                    e.statusCode(), e.getMessage());
        }
    }

    // ---- /link (spec §10 first leg) ----

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
        boolean zh = zh(player);
        player.sendMessage(Component.text(
                zh ? "正在获取绑定码……" : "Requesting a link code…", NamedTextColor.GRAY));
        async(() -> {
            try {
                LinkCode code = linkClient.requestCode(player.getUniqueId());
                player.sendMessage(Component.text(
                        zh ? "你的绑定码：" : "Your link code: ", NamedTextColor.GREEN)
                        .append(Component.text(code.code(), NamedTextColor.YELLOW)));
                String panelUrl = code.panelUrl();
                if (panelUrl != null) {
                    player.sendMessage(Component.text(
                            zh ? "在这里输入它完成绑定（几分钟内有效）："
                               : "Enter it here to finish linking (valid a few minutes):",
                            NamedTextColor.GRAY));
                    player.sendMessage(Component.text("  " + panelUrl, NamedTextColor.WHITE));
                } else {
                    player.sendMessage(Component.text(
                            zh ? "在网页控制台 → 账户 中输入它完成绑定（几分钟内有效）。"
                               : "Enter it on the web console → Account to finish linking (valid a few minutes).",
                            NamedTextColor.GRAY));
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

    // ---- /felis command suite (spec §B in-game authz; P4) ----
    //
    // /felis is the proxy-wide operator surface: the ONE place a command runs with a
    // service token behind it AND a Mojang-verified UUID in front of it, so it is where
    // the in-game half of the passwordless-auth flows lives. The tree:
    //
    //   /felis                        proxy + routing status
    //   /felis help                   this list
    //   /felis server                 the felis servers this proxy knows
    //   /felis go <server>            wake a server and move me in when it's ready
    //   /felis claim                  take ownership of the server I'm on
    //   /felis migrate                open a migration of my servers to another account
    //   /felis web                    where the web consoles live
    //   /felis web op approve <code>  vouch for a pending op.console staff login (§B)
    //
    // Two guards run before any subcommand that acts or reveals operational state:
    //
    //   - the login-limbo gate — a player still sitting in the "login" system server
    //     (naming.SystemLoginServer) has not passed the front door, so every acting
    //     subcommand is refused there; only `help` is always available. The console
    //     source is the trusted operator terminal and is never "in limbo".
    //   - player-only actions (go / claim / web op approve) reject the console: they
    //     derive identity from the caller's verified UUID, which only a Player carries,
    //     so an op-login approver is provably online as themselves (spec §14). The
    //     API independently re-checks that the UUID is a linked admin — this gate is
    //     defence in depth over that, not a substitute for it.

    /** Opaque-handle charset an op-login code must match before it enters a request
     *  path: the id minted by op-login start is 32 hex, but a conservative url-safe set
     *  is accepted so a future id format still passes while path-dangerous input
     *  (slash, dot, whitespace) is refused client-side rather than sent. */
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
                                                    doOpApprove(ctx.getSource(), StringArgumentType.getString(ctx, "code"));
                                                    return Command.SINGLE_SUCCESS;
                                                })))))
                .build();
        CommandMeta meta = commands.metaBuilder("felis").plugin(this).build();
        commands.register(meta, new BrigadierCommand(node));
    }

    // ---- guards ----

    // requirePlayer refuses the console for identity-bound actions (go / claim /
    // approve): they act on the caller's Mojang-verified UUID, which only a Player has.
    private Player requirePlayer(CommandSource source) {
        if (source instanceof Player) {
            return (Player) source;
        }
        source.sendMessage(Component.text("That command can only be run in-game by a player.", NamedTextColor.RED));
        return null;
    }

    // ensureOutOfLimbo refuses an acting subcommand while the player is still in the
    // login limbo (or not yet on any backend): they have not passed the front door.
    // Fails closed on an unknown position.
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

    // gateInfo applies only the limbo gate (the console is always allowed) for the
    // read-only, operational-info subcommands. Returns true when the caller may proceed.
    private boolean gateInfo(CommandSource source) {
        return !(source instanceof Player) || ensureOutOfLimbo((Player) source);
    }

    // ---- handlers ----

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
        helpLine(source, "/felis claim",
                zh ? "认领你所在的服务器" : "take ownership of the server you're on");
        helpLine(source, "/felis migrate",
                zh ? "把你的服务器迁移到另一个账户" : "move your servers to another account");
        helpLine(source, "/felis web",
                zh ? "网页控制台地址" : "where the web consoles live");
        helpLine(source, "/felis web op approve <code>",
                zh ? "批准待处理的管理员登录" : "approve a pending operator sign-in");
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

    /**
     * doGo parks the caller on the server they named. Returns whether the request reached
     * the waiting queue — false means a guard refused it and told the player why, which is
     * what {@link #doInviteAnswer} reports back to the inviter instead of guessing.
     *
     * <p>{@code joinIfReady} is set only by an accepted invite: see
     * {@link WaitingRouter#enqueueFromInvite}.
     */
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
        // Wake + park + transfer through the shared waiting queue; it reports its own
        // policy-gate (403) and transient refusals to the player.
        if (joinIfReady) {
            router.enqueueFromInvite(player, match.name());
        } else {
            router.enqueueFromCommand(player, match.name());
        }
        return true;
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
        UUID uuid = player.getUniqueId();
        player.sendMessage(Component.text(
                zh ? "正在认领「" + name + "」……" : "Claiming « " + name + " »…", NamedTextColor.GRAY));
        async(() -> {
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

    // doMigrate opens an account migration for the calling player (spec §B3 inherit): it
    // hands their owned servers to another account. Identity-bound (acts on the caller's
    // verified UUID) and out-of-limbo like claim, but server-independent — it touches the
    // account, not the server the player stands on, so there is no registry/current-server
    // check. The command only OPENS the migration; the player finishes it on the web
    // console (prove it's them, name the receiving account, redeem a code), so on success
    // we point them there.
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
        UUID uuid = player.getUniqueId();
        String who = player.getUsername();
        player.sendMessage(Component.text(
                zh ? "正在发起账户迁移……" : "Starting account migration…", NamedTextColor.GRAY));
        async(() -> {
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
                zh ? "  管理员：/felis web op approve <code> 用于为待处理登录作担保"
                   : "  operators: /felis web op approve <code> vouches for a pending sign-in",
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
                zh ? "即可为其担保——你必须是已绑定并在线的管理员。"
                   : "to vouch for it — you must be an online, linked administrator.",
                NamedTextColor.GRAY));
    }

    private void doOpApprove(CommandSource source, String codeArg) {
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
        UUID approver = player.getUniqueId();
        String who = player.getUsername();
        player.sendMessage(Component.text(
                zh ? "正在批准管理员登录……" : "Approving operator sign-in…", NamedTextColor.GRAY));
        async(() -> {
            try {
                apiClient.opLoginApprove(code, approver);
                player.sendMessage(Component.text(
                        zh ? "已批准——对方现在可以完成登录了。"
                           : "Approved — the operator can finish signing in now.", NamedTextColor.GREEN));
                logger.info("Felis: op-login {} approved in-game by {} ({})", code, who, approver);
            } catch (LinkException e) {
                player.sendMessage(Component.text(opApproveError(e, zh), NamedTextColor.RED));
            }
        });
    }

    // ---- /invite (bring another player to the server you're on) ----
    //
    // /invite is a UX wrapper over `/felis go`, and nothing more: Accept runs the same doGo
    // path on the ACCEPTING player's own verified uuid, so it fills in the name of a place
    // the invitee could already reach unaided. You can only invite someone to the server you
    // are standing on, so the target is RUNNING — and a running felis server already admits
    // any linked player through <name>.<root-domain> on the link check alone (WaitingRouter
    // .onServerPreConnect). An invite therefore hands over no access the invitee lacked: at
    // worst a stale or guessed accept sends you somewhere you could have walked yourself,
    // which is why a stored invite carries only a server name and never an identity to act
    // as, and why the prompt needs no unguessable token.
    //
    // It is NOT consequence-free, though, and the inviter is told so. Landing on a felis
    // server records the player in its allowlist (WaitingRouter.onServerConnected -> the
    // join-event -> RecordJoin), and on an autostartPolicy=allowlist server that record is
    // what lets them come back later and START the thing. Same record they would earn by
    // walking in themselves, so this is not an escalation — but it outlives the invite, so
    // it belongs on screen at send time rather than in a comment only we read.
    //
    // The remaining new capability is "make a chat card appear on any online player", which
    // is rate-limited per sender by InviteBook rather than left to good manners.
    //
    // The prompt renders in the INVITEE's language (they are the one being asked) while
    // the inviter's confirmations follow theirs.

    private void registerInviteCommand() {
        CommandManager commands = proxy.getCommandManager();
        LiteralCommandNode<CommandSource> node = BrigadierCommand.literalArgumentBuilder("invite")
                .executes(ctx -> {
                    sendInviteUsage(ctx.getSource());
                    return Command.SINGLE_SUCCESS;
                })
                // The optional <server> is what the card's buttons carry: it pins a click to
                // the invite that drew it, so an old card cannot answer a newer invite. Typed
                // bare, both still answer whatever is pending.
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
                // Brigadier matches literals before arguments, so a player
                // actually named "accept"/"deny" cannot be invited by name. They can
                // still reach the server with /felis go, and renaming the subcommands
                // would break the click handlers for a case worth less than that.
                .then(BrigadierCommand.requiredArgumentBuilder("player", StringArgumentType.word())
                        .suggests((ctx, builder) -> {
                            // Brigadier does not filter suggestions for us: without the
                            // prefix test every keystroke re-offers the whole proxy roster.
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
        // You can only invite someone to where you already are, so the invite names a
        // server the inviter is demonstrably on rather than any server they can spell.
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
        // A player still at the login gate can see the card but not answer it — doInviteAnswer's
        // own limbo guard would refuse the click. Refuse here instead, so an invite is never a
        // button that does nothing, and the inviter learns why rather than waiting for silence.
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

        // Last gate, so that every invite refused above stays free: the cooldown exists to
        // stop cards being sprayed at players, and a refusal sends no card.
        long now = System.currentTimeMillis();
        long wait = invites.cooldownRemaining(inviter.getUniqueId(), now);
        if (wait > 0) {
            long secs = (wait + 999) / 1000; // round up: "0 秒后再试" would be a lie
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

    /**
     * accessNotice tells the inviter what the invite costs them, because it is not nothing:
     * a player who accepts and lands on the server is written into its allowlist by the join
     * event, exactly as if they had walked in on their own.
     *
     * <p>What that record is WORTH depends on the server's autostartPolicy, so the wording
     * does too. Under {@code allowlist} it is durable authority — that row is what lets them
     * start the server themselves later — and the inviter is told plainly. Under any other
     * policy (including the {@code ownerOnly} default, and the empty string the API reports
     * when the field was never set) the row grants no waking, so claiming it did would be a
     * lie; there it says only that they were recorded.
     */
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

    // The card itself lives in InviteCard so the buttons — the whole point of the feature —
    // can be asserted without a live proxy. All this does is address it.
    private void sendInviteCard(Player invitee, String inviterName, String server) {
        InviteCard.lines(inviterName, server, zh(invitee), INVITE_TTL.toSeconds())
                .forEach(invitee::sendMessage);
    }

    /**
     * doInviteAnswer handles both buttons. {@code fromCard} is the server the clicked card
     * named, or null when the player typed the subcommand bare.
     */
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
        // Checked before consuming: a click on a card a later invite superseded must leave
        // the live invite alone, so the player can still answer the card that is current.
        if (fromCard != null && !fromCard.equalsIgnoreCase(pending.server())) {
            player.sendMessage(Component.text(
                    zh ? "这张邀请卡已被新的邀请取代——你当前的邀请是前往「" + pending.server() + "」。"
                       : "That invite was superseded — your pending one is to « " + pending.server() + " ».",
                    NamedTextColor.YELLOW));
            return;
        }
        // Peek-then-take is not atomic — an invite landing in that window is
        // taken instead of the one just validated. "Newest wins" is already the rule the
        // book enforces, so the outcome is one this player would have got anyway; make it
        // a computeIfPresent if invites ever arrive fast enough for anyone to notice.
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
        // Accept IS `/felis go` with the name filled in — doGo re-runs every guard: routing
        // active, the server still registered and non-system, not already there. It differs
        // only in joining a server that is already up rather than asking to wake it; see
        // WaitingRouter.enqueueFromInvite for why that is the difference between a working
        // button and a 403.
        //
        // Reported to the inviter AFTER the handoff, not on the click: telling them "accepted"
        // while their guest is being turned away is worse than telling them nothing.
        notifyInviter(invite, player.getUsername(),
                doGo(player, invite.server(), true) ? Answer.ACCEPTED : Answer.FAILED);
    }

    private enum Answer { ACCEPTED, DECLINED, FAILED }

    // notifyInviter closes the loop for whoever sent the invite; without it they wait on a
    // prompt they can never see the answer to. Silently skipped if they left in the meantime.
    //
    // ACCEPTED means the transfer was handed to the waiting queue, which is as far
    // as this can see synchronously — a wake that fails later is reported to the guest only.
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

    // ---- helpers ----

    /**
     * zh reports whether the caller's client locale is Chinese, so player-facing
     * text can follow the client language. The console (and any non-player source)
     * always reads English, and a client that has not yet sent its settings falls
     * back to English too. Package-private so {@link WaitingRouter} and the other
     * proxy faces share the one locale rule.
     */
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

    // claimError maps the felis-api claim refusals (spec §9.3) to player-safe text.
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

    // migrateError maps the felis-api migrate-start refusals (spec §B3) to player-safe
    // text. A 404 means the caller's UUID isn't linked to any account to migrate; a 409
    // means the linked account can't start one (already migrated, or retired).
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

    // opApproveError maps the internal approve refusals to player-safe text. A 403 is
    // the API's own admin re-check (defence in depth over the in-game gate); a 404
    // means no live pending request carries that code.
    private static String opApproveError(LinkException e, boolean zh) {
        switch (e.statusCode()) {
            case 403:
                return zh ? "只有已绑定的管理员才能批准管理员登录。"
                          : "Only a linked administrator may approve an operator sign-in.";
            case 404:
                return zh ? "没有携带该码的待处理管理员登录（可能已过期）。"
                          : "No pending operator sign-in with that code (it may have expired).";
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
