package best.lolicon.felis.limbo;

import best.lolicon.felis.link.Control;
import best.lolicon.felis.link.FelisApiClient;
import best.lolicon.felis.link.LinkClient;
import best.lolicon.felis.link.LinkCode;
import best.lolicon.felis.link.LinkConfig;
import best.lolicon.felis.link.ExperienceConfig;
import best.lolicon.felis.link.LinkConfigLoader;

import com.loohp.limbo.events.EventHandler;
import com.loohp.limbo.events.Listener;
import com.loohp.limbo.events.player.PlayerJoinEvent;
import com.loohp.limbo.player.Player;
import com.loohp.limbo.plugins.LimboPlugin;
import com.loohp.limbo.scheduler.LimboScheduler;

import net.kyori.adventure.key.Key;

import java.io.File;
import java.io.IOException;
import java.util.UUID;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * FelisLimboPlugin turns a LOOHP/Limbo instance into the Felis <b>login gate</b>
 * (spec §B3): the always-on front door every fresh connection lands on. It does two
 * jobs.
 *
 * <p><b>1. Readiness.</b> Limbo has no RCON, so the operator cannot use its usual
 * RCON readiness probe. Left to a plain TCP check the pod would report ready the
 * instant the socket binds. This plugin serves {@code GET /healthz} on
 * {@code FELIS_HEALTH_PORT} (default 8080) that flips from 503 to 200 only after the
 * first server tick ({@link Readiness}) — the MinecraftServer CRD's HTTP
 * readinessProbe follows that true signal. Fail-closed: if the endpoint cannot bind,
 * readiness never turns green and the operator keeps the gate in Starting rather than
 * advertising an unstarted auth gate.
 *
 * <p><b>2. In-game login.</b> A player who reaches the limbo has been UUID-verified
 * upstream (Velocity online-mode) but is not yet linked to a web account. On join
 * {@link LoginFlow}, off the tick thread:
 * <ol>
 *   <li>checks the username-collision blacklist and disconnects a barred squatter
 *       UUID (the genuine Mojang player, different UUID, passes);</li>
 *   <li>mints a one-time Bind Code for the verified UUID via the felis-api internal
 *       face ({@link LinkClient});</li>
 *   <li>opens a book (clickable link to {@code console.<root_domain>}) plus a chat
 *       line carrying the code, and tells the player to finish in their <em>system</em>
 *       browser — never the WeChat/QQ in-app browser, where WebAuthn/passkey does not
 *       work (the web entry additionally guards this, see internal/panel);</li>
 *   <li>polls {@code link/status/{uuid}} until the player redeems the code on the web
 *       console, then asks the proxy to move them to the lobby with a
 *       {@code felis:control} {@code LoginRelease} frame, re-sent with backoff until
 *       the player has left (the proxy re-checks the link before it lets them go);</li>
 *   <li>disconnects (fail-closed) on blacklist, on a refused mint, or when the login
 *       window elapses — "rather refuse than admit unauthenticated".</li>
 * </ol>
 *
 * <p>A felis-api outage at join (transport error or 5xx) is retried with backoff for
 * {@link LoginFlow#START_RETRY_WINDOW_MILLIS} before the player is turned away. Each
 * retry also sends a {@code LoginRelease}: the proxy keeps recent link confirmations
 * for exactly this case, so a player who was signed in minutes ago can still get
 * through a felis-api restart, and anyone else is refused there.
 *
 * <p>The release used to be a BungeeCord {@code Connect} on {@code bungeecord:main}.
 * Velocity answers that channel itself, before any plugin can see it, so leaving it
 * on for the gate left it on for every user backend (KickPlayer, ConnectOther). The
 * installer now switches it off and the proxy accepts {@code LoginRelease} only from
 * this server.
 *
 * <p><b>Config (deployment inputs, never compiled in).</b> The API base URL and
 * service token come from {@code FELIS_API_BASE_URL} / {@code FELIS_SERVICE_TOKEN}
 * (env wins, else a {@code felis-link.properties} template in the plugin data dir) via
 * the shared {@link LinkConfigLoader}. {@code FELIS_ROOT_DOMAIN} builds the console
 * link; {@code FELIS_LOBBY_SERVER} (default {@code lobby}) is the transfer target;
 * {@code FELIS_LOGIN_TIMEOUT_SECONDS} (default 600) bounds the login window
 * ({@link GateConfig}). If the link config or the root domain is absent the login
 * flow stays OFF and the plugin runs readiness-only — the same "load un-crippled"
 * fail-safe the other Felis plugins use — so a bare image still boots and serves
 * readiness; production must supply the config for the gate to authenticate.
 *
 * <p>This class is the Limbo glue only. The flow, the readiness endpoint, the env
 * parsing and the book live in plain classes that {@code ./gradlew loginTest} (run by
 * plugins/test.sh) drives on a virtual clock against a stub felis-api.
 */
public final class FelisLimboPlugin extends LimboPlugin implements Listener {

    private static final Logger LOG = Logger.getLogger("FelisLimbo");
    // Limbo deprecated the String channel overload; its Key overload sends key.toString(),
    // which for "felis:control" is the same channel string.
    private static final Key CONTROL_CHANNEL = Key.key(Control.CHANNEL);

    private volatile Readiness readiness;
    private volatile LoginFlow flow;
    private ExperienceConfig experience;

    @Override
    public void onEnable() {
        startReadiness();
        configureLogin();
        if (flow != null) {
            getServer().getEventsManager().registerEvents(this, this);
        }
    }

    @Override
    public void onDisable() {
        Readiness r = this.readiness;
        if (r != null) {
            r.stop();
            this.readiness = null;
        }
        // cancelTask(plugin) tears down every scheduled task this plugin owns.
        getServer().getScheduler().cancelTask(this);
        LoginFlow f = this.flow;
        if (f != null) {
            f.clear();
        }
    }

    private void startReadiness() {
        int port = GateConfig.healthPort(System.getenv("FELIS_HEALTH_PORT"));
        Readiness r;
        try {
            r = Readiness.start(port);
        } catch (IOException e) {
            // Fail closed: no health server → never ready → pod stays NotReady.
            LOG.log(Level.SEVERE, "FelisLimbo: could not start readiness endpoint on :" + port
                    + " — the login gate will stay NotReady", e);
            return;
        }
        this.readiness = r;
        LOG.info("FelisLimbo: readiness endpoint on :" + port + Readiness.PATH + " (503 until the first tick)");

        // onEnable runs just before Limbo opens its game socket and no "started"
        // event exists, so mark ready one tick later: by the time the scheduler runs
        // the task the server loop is ticking and the socket is open.
        getServer().getScheduler().runTaskLater(this, () -> {
            if (r.markReady()) {
                LOG.info("FelisLimbo: server started — readiness now 200");
            }
        }, 1L);
    }

    private void configureLogin() {
        LinkConfig cfg;
        try {
            File props = new File(getDataFolder(), "felis-link.properties");
            cfg = LinkConfigLoader.load(props.toPath());
            experience = ExperienceConfig.load(ExperienceConfig.PATH);
        } catch (IOException e) {
            // Missing/half config: like the other Felis plugins, load un-crippled —
            // readiness stays up, the login flow just never turns on.
            LOG.warning("FelisLimbo: " + e.getMessage());
            logLoginOff();
            return;
        }
        String panelHost = GateConfig.panelHost(System.getenv("FELIS_PANEL_HOSTNAME"), System.getenv("FELIS_ROOT_DOMAIN"));
        if (panelHost == null) {
            LOG.warning("FelisLimbo: neither FELIS_PANEL_HOSTNAME nor FELIS_ROOT_DOMAIN set — cannot build the console login link");
            logLoginOff();
            return;
        }
        String consoleUrl = "https://" + panelHost;
        String lobby = GateConfig.lobby(System.getenv("FELIS_LOBBY_SERVER"));
        long timeoutMillis = GateConfig.loginTimeoutSeconds(System.getenv("FELIS_LOGIN_TIMEOUT_SECONDS") != null
                ? System.getenv("FELIS_LOGIN_TIMEOUT_SECONDS")
                : Long.toString(experience.number("loginTimeoutSeconds", GateConfig.DEFAULT_TIMEOUT_SECONDS))) * 1000L;
        this.flow = new LoginFlow(new LimboGate(), new FelisApiClient(cfg), new LinkClient(cfg),
                consoleUrl, timeoutMillis, System::currentTimeMillis, LOG);
        LOG.info("FelisLimbo: login flow ON — console=" + consoleUrl
                + ", lobby=" + lobby + ", window=" + (timeoutMillis / 1000) + "s");
    }

    private static void logLoginOff() {
        LOG.warning("FelisLimbo: login flow OFF (missing FELIS_API_BASE_URL/FELIS_SERVICE_TOKEN "
                + "or FELIS_ROOT_DOMAIN) — serving readiness only; this gate will NOT authenticate players");
    }

    @EventHandler
    public void onJoin(PlayerJoinEvent event) {
        flow.join(event.getPlayer().getUniqueId());
    }

    /** LimboGate is the flow's view of this server: its scheduler and its players. */
    private final class LimboGate implements LoginFlow.Gate {
        private LimboScheduler scheduler() {
            return getServer().getScheduler();
        }

        @Override
        public LoginFlow.Seat player(UUID id) {
            Player player = getServer().getPlayer(id);
            return player != null && player.isValid() ? new LimboSeat(player, experience) : null;
        }

        @Override
        public int runSync(Runnable task) {
            return scheduler().runTask(FelisLimboPlugin.this, task::run);
        }

        @Override
        public int runAsync(Runnable task) {
            return scheduler().runTaskAsync(FelisLimboPlugin.this, task::run);
        }

        @Override
        public int runLater(Runnable task, long delayTicks) {
            return scheduler().runTaskLater(FelisLimboPlugin.this, task::run, delayTicks);
        }

        @Override
        public int runLaterAsync(Runnable task, long delayTicks) {
            return scheduler().runTaskLaterAsync(FelisLimboPlugin.this, task::run, delayTicks);
        }

        @Override
        public int runTimerAsync(Runnable task, long delayTicks, long periodTicks) {
            return scheduler().runTaskTimerAsync(FelisLimboPlugin.this, task::run, delayTicks, periodTicks);
        }

        @Override
        public void cancel(int taskId) {
            scheduler().cancelTask(taskId);
        }
    }

    /** LimboSeat is one Limbo player as the flow sees them. */
    private record LimboSeat(Player player, ExperienceConfig experience) implements LoginFlow.Seat {
        @Override
        public String name() {
            return player.getName();
        }

        @Override
        public void chat(String line) {
            player.sendMessage(LoginBook.chatLine(line));
        }

        @Override
        public void showCode(LinkCode code, String url) {
            if (experience.flag("openBook", true)) {
                player.openBook(LoginBook.book(code.code(), url, experience));
            }
        }

        @Override
        public void sendControl(byte[] frame) throws IOException {
            player.sendPluginMessage(CONTROL_CHANNEL, frame);
        }

        @Override
        public void disconnect(String reason) {
            player.disconnect(reason);
        }
    }
}
