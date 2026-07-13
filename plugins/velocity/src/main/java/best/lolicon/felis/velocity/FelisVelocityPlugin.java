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

    private final ProxyServer proxy;
    private final Logger logger;
    private final Path dataDirectory;

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
        logger.info("Felis routing ready: rootDomain={}, login={}, lobby={}. /link and /felis registered.",
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
        player.sendMessage(Component.text("Requesting a link code…", NamedTextColor.GRAY));
        async(() -> {
            try {
                LinkCode code = linkClient.requestCode(player.getUniqueId());
                player.sendMessage(Component.text("Your link code: ", NamedTextColor.GREEN)
                        .append(Component.text(code.code(), NamedTextColor.YELLOW)));
                player.sendMessage(Component.text(
                        "Enter it on the web panel → Account to finish linking (valid a few minutes).",
                        NamedTextColor.GRAY));
            } catch (LinkException e) {
                logger.warn("link code request failed for {} (status={}, code={}): {}",
                        player.getUniqueId(), e.statusCode(), e.errorCode(), e.getMessage());
                player.sendMessage(Component.text(
                        "Couldn't get a link code right now. Please try again in a moment.",
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
                                    doGo(ctx.getSource(), StringArgumentType.getString(ctx, "server"));
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
        Optional<ServerConnection> current = player.getCurrentServer();
        if (current.isEmpty()) {
            player.sendMessage(Component.text(
                    "Hold on — finish connecting before using /felis.", NamedTextColor.YELLOW));
            return false;
        }
        if (config.loginServer().equalsIgnoreCase(current.get().getServerInfo().getName())) {
            player.sendMessage(Component.text(
                    "Finish signing in first — /felis isn't available from the login area.",
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
        source.sendMessage(Component.text("Felis proxy", NamedTextColor.AQUA));
        source.sendMessage(field("online-mode", String.valueOf(onlineMode)));
        if (!routingActive) {
            source.sendMessage(Component.text(
                    "  routing: disabled" + (onlineMode ? " (no root-domain set)" : " (offline mode)"),
                    NamedTextColor.YELLOW));
            source.sendMessage(Component.text("  /felis help for commands", NamedTextColor.GRAY));
            return;
        }
        source.sendMessage(field("root-domain", config.rootDomain()));
        source.sendMessage(field("login", config.loginServer()));
        source.sendMessage(field("lobby", config.lobbyServer()));
        source.sendMessage(field("servers", String.valueOf(registry.all().size())));
        source.sendMessage(Component.text("  /felis help for commands", NamedTextColor.GRAY));
    }

    private void sendHelp(CommandSource source) {
        source.sendMessage(Component.text("Felis commands", NamedTextColor.AQUA));
        helpLine(source, "/felis", "proxy and routing status");
        helpLine(source, "/felis server", "the felis servers this proxy knows");
        helpLine(source, "/felis go <server>", "start a server and move you in when it's ready");
        helpLine(source, "/felis claim", "take ownership of the server you're on");
        helpLine(source, "/felis migrate", "move your servers to another account");
        helpLine(source, "/felis web", "where the web consoles live");
        helpLine(source, "/felis web op approve <code>", "approve a pending operator sign-in");
    }

    private void sendServerList(CommandSource source) {
        if (!gateInfo(source)) {
            return;
        }
        if (!routingActive) {
            source.sendMessage(Component.text("Felis routing is disabled.", NamedTextColor.YELLOW));
            return;
        }
        List<ServerView> servers = registry.all().stream()
                .filter(v -> !isSystemServer(v.name()))
                .toList();
        if (servers.isEmpty()) {
            source.sendMessage(Component.text("No felis servers known yet.", NamedTextColor.GRAY));
            return;
        }
        source.sendMessage(Component.text("Felis servers:", NamedTextColor.AQUA));
        for (ServerView v : servers) {
            String phase = v.phase() == null ? "?" : v.phase();
            source.sendMessage(Component.text("  " + v.name() + " ", NamedTextColor.WHITE)
                    .append(Component.text("[" + phase + (v.ready() ? ", ready" : "") + "]",
                            v.ready() ? NamedTextColor.GREEN : NamedTextColor.GRAY)));
        }
    }

    private void doGo(CommandSource source, String serverArg) {
        Player player = requirePlayer(source);
        if (player == null || !ensureOutOfLimbo(player)) {
            return;
        }
        if (!routingActive) {
            player.sendMessage(routingDisabled());
            return;
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
                    "No felis server named « " + target + " ». Try /felis server.", NamedTextColor.YELLOW));
            return;
        }
        Optional<ServerConnection> current = player.getCurrentServer();
        if (current.isPresent() && current.get().getServerInfo().getName().equalsIgnoreCase(match.name())) {
            player.sendMessage(Component.text("You're already on « " + match.name() + " ».", NamedTextColor.GRAY));
            return;
        }
        // Wake + park + transfer through the shared waiting queue; it reports its own
        // policy-gate (403) and transient refusals to the player.
        router.enqueueFromCommand(player, match.name());
    }

    private void doClaim(CommandSource source) {
        Player player = requirePlayer(source);
        if (player == null || !ensureOutOfLimbo(player)) {
            return;
        }
        if (!routingActive) {
            player.sendMessage(routingDisabled());
            return;
        }
        Optional<ServerConnection> current = player.getCurrentServer();
        if (current.isEmpty()) {
            player.sendMessage(Component.text("Join a server before claiming it.", NamedTextColor.YELLOW));
            return;
        }
        String name = current.get().getServerInfo().getName();
        if (!registry.isManaged(name)) {
            player.sendMessage(Component.text(
                    "« " + name + " » isn't a claimable felis server.", NamedTextColor.YELLOW));
            return;
        }
        UUID uuid = player.getUniqueId();
        player.sendMessage(Component.text("Claiming « " + name + " »…", NamedTextColor.GRAY));
        async(() -> {
            try {
                apiClient.claim(name, uuid);
                player.sendMessage(Component.text("You now own « " + name + " ».", NamedTextColor.GREEN));
            } catch (LinkException e) {
                player.sendMessage(Component.text(claimError(e, name), NamedTextColor.RED));
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
        if (!routingActive) {
            player.sendMessage(routingDisabled());
            return;
        }
        UUID uuid = player.getUniqueId();
        String who = player.getUsername();
        player.sendMessage(Component.text("Starting account migration…", NamedTextColor.GRAY));
        async(() -> {
            try {
                apiClient.migrateStart(uuid);
                String root = config.rootDomain();
                player.sendMessage(Component.text(
                        "Migration started — finish it on the web console:", NamedTextColor.GREEN));
                player.sendMessage(Component.text(
                        "  " + (root == null ? "the players' web console" : "https://console." + root),
                        NamedTextColor.WHITE));
                player.sendMessage(Component.text(
                        "You'll confirm it's you, name the account to receive your servers, then get a code.",
                        NamedTextColor.GRAY));
                logger.info("Felis: account migration started in-game by {} ({})", who, uuid);
            } catch (LinkException e) {
                player.sendMessage(Component.text(migrateError(e), NamedTextColor.RED));
            }
        });
    }

    private void sendWebInfo(CommandSource source) {
        if (!gateInfo(source)) {
            return;
        }
        String root = config.rootDomain();
        if (root == null) {
            source.sendMessage(Component.text(
                    "The web console isn't configured on this proxy.", NamedTextColor.YELLOW));
            return;
        }
        source.sendMessage(Component.text("Felis web consoles", NamedTextColor.AQUA));
        source.sendMessage(field("players", "https://console." + root));
        source.sendMessage(field("operators", "https://op.console." + root));
        source.sendMessage(Component.text(
                "  operators: /felis web op approve <code> vouches for a pending sign-in",
                NamedTextColor.GRAY));
    }

    private void sendWebOpInfo(CommandSource source) {
        if (!gateInfo(source)) {
            return;
        }
        source.sendMessage(Component.text("Operator sign-in", NamedTextColor.AQUA));
        source.sendMessage(Component.text(
                "An operator signing in at op.console shows an approval code. Run", NamedTextColor.GRAY));
        source.sendMessage(Component.text("  /felis web op approve <code>", NamedTextColor.WHITE));
        source.sendMessage(Component.text(
                "to vouch for it — you must be an online, linked administrator.", NamedTextColor.GRAY));
    }

    private void doOpApprove(CommandSource source, String codeArg) {
        Player player = requirePlayer(source);
        if (player == null || !ensureOutOfLimbo(player)) {
            return;
        }
        if (!routingActive) {
            player.sendMessage(routingDisabled());
            return;
        }
        String code = codeArg.trim();
        if (!OP_LOGIN_CODE.matcher(code).matches()) {
            player.sendMessage(Component.text(
                    "That doesn't look like a valid approval code.", NamedTextColor.RED));
            return;
        }
        UUID approver = player.getUniqueId();
        String who = player.getUsername();
        player.sendMessage(Component.text("Approving operator sign-in…", NamedTextColor.GRAY));
        async(() -> {
            try {
                apiClient.opLoginApprove(code, approver);
                player.sendMessage(Component.text(
                        "Approved — the operator can finish signing in now.", NamedTextColor.GREEN));
                logger.info("Felis: op-login {} approved in-game by {} ({})", code, who, approver);
            } catch (LinkException e) {
                player.sendMessage(Component.text(opApproveError(e), NamedTextColor.RED));
            }
        });
    }

    // ---- helpers ----

    private Component routingDisabled() {
        return Component.text(
                "Felis routing is disabled on this proxy" + (onlineMode ? " (no root-domain set)." : " (offline mode)."),
                NamedTextColor.YELLOW);
    }

    private boolean isSystemServer(String name) {
        return config.loginServer().equalsIgnoreCase(name)
                || config.lobbyServer().equalsIgnoreCase(name);
    }

    // claimError maps the felis-api claim refusals (spec §9.3) to player-safe text.
    private static String claimError(LinkException e, String server) {
        switch (e.statusCode()) {
            case 412:
                return "Link your account on the web console before claiming a server.";
            case 403:
                return "You've reached your server limit — you can't claim another.";
            case 409:
                return "« " + server + " » is already owned.";
            case 404:
                return "« " + server + " » is no longer available.";
            case 0:
                return "Felis is temporarily unavailable — please try again.";
            default:
                return "Couldn't claim « " + server + " » right now. Please try again.";
        }
    }

    // migrateError maps the felis-api migrate-start refusals (spec §B3) to player-safe
    // text. A 404 means the caller's UUID isn't linked to any account to migrate; a 409
    // means the linked account can't start one (already migrated, or retired).
    private static String migrateError(LinkException e) {
        switch (e.statusCode()) {
            case 404:
                return "Link your account on the web console before migrating.";
            case 409:
                return "This account can't start a migration (already migrated or retired).";
            case 0:
                return "Felis is temporarily unavailable — please try again.";
            default:
                return "Couldn't start the migration right now. Please try again.";
        }
    }

    // opApproveError maps the internal approve refusals to player-safe text. A 403 is
    // the API's own admin re-check (defence in depth over the in-game gate); a 404
    // means no live pending request carries that code.
    private static String opApproveError(LinkException e) {
        switch (e.statusCode()) {
            case 403:
                return "Only a linked administrator may approve an operator sign-in.";
            case 404:
                return "No pending operator sign-in with that code (it may have expired).";
            case 0:
                return "Felis is temporarily unavailable — please try again.";
            default:
                return "Couldn't approve that sign-in right now. Please try again.";
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
