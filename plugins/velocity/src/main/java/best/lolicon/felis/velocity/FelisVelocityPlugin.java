package best.lolicon.felis.velocity;

import best.lolicon.felis.link.FelisApiClient;
import best.lolicon.felis.link.LinkClient;
import best.lolicon.felis.link.LinkCode;
import best.lolicon.felis.link.LinkException;
import best.lolicon.felis.link.ServerView;

import com.google.inject.Inject;
import com.mojang.brigadier.Command;
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
import net.kyori.adventure.text.Component;
import net.kyori.adventure.text.format.NamedTextColor;
import org.slf4j.Logger;

import java.nio.file.Path;
import java.time.Duration;
import java.util.Collection;
import java.util.List;

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
        WaitingRouter router = new WaitingRouter(proxy, logger, apiClient, registry, this, config.lobbyServer());
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
        if (config.lobbyServer() == null) {
            logger.warn("Felis routing active without a lobby-server: a player whose target is asleep will be "
                    + "asked to reconnect rather than parked. Set 'lobby-server=' to enable the waiting queue.");
        }
        logger.info("Felis routing ready: rootDomain={}, lobby={}. /link and /felis registered.",
                config.rootDomain(), config.lobbyServer() == null ? "<none>" : config.lobbyServer());
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

    // ---- /felis (operator status) ----

    private void registerFelisCommand() {
        CommandManager commands = proxy.getCommandManager();
        LiteralCommandNode<CommandSource> node = BrigadierCommand.literalArgumentBuilder("felis")
                .executes(ctx -> {
                    sendSummary(ctx.getSource());
                    return Command.SINGLE_SUCCESS;
                })
                .then(BrigadierCommand.literalArgumentBuilder("list")
                        .executes(ctx -> {
                            sendList(ctx.getSource());
                            return Command.SINGLE_SUCCESS;
                        }))
                .build();
        CommandMeta meta = commands.metaBuilder("felis").plugin(this).build();
        commands.register(meta, new BrigadierCommand(node));
    }

    private void sendSummary(CommandSource source) {
        source.sendMessage(Component.text("Felis proxy", NamedTextColor.AQUA));
        source.sendMessage(field("online-mode", String.valueOf(onlineMode)));
        if (!routingActive) {
            source.sendMessage(Component.text(
                    "  routing: disabled" + (onlineMode ? " (no root-domain set)" : " (offline mode)"),
                    NamedTextColor.YELLOW));
            return;
        }
        source.sendMessage(field("root-domain", config.rootDomain()));
        source.sendMessage(field("lobby", config.lobbyServer() == null ? "<none>" : config.lobbyServer()));
        source.sendMessage(field("servers", String.valueOf(registry.all().size())));
    }

    private void sendList(CommandSource source) {
        if (!routingActive) {
            source.sendMessage(Component.text("Felis routing is disabled.", NamedTextColor.YELLOW));
            return;
        }
        Collection<ServerView> servers = registry.all();
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

    private static Component field(String key, String value) {
        return Component.text("  " + key + ": ", NamedTextColor.GRAY)
                .append(Component.text(value == null ? "<unset>" : value, NamedTextColor.WHITE));
    }
}
