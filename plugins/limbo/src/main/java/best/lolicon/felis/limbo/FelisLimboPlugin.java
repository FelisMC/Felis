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

/** Felis login gate on Limbo: serves {@code /healthz} readiness and runs {@link LoginFlow}. The flow stays off (readiness only) when the link config or root domain is missing. */
public final class FelisLimboPlugin extends LimboPlugin implements Listener {

    private static final Logger LOG = Logger.getLogger("FelisLimbo");
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
            LOG.log(Level.SEVERE, "FelisLimbo: could not start readiness endpoint on :" + port
                    + " — the login gate will stay NotReady", e);
            return;
        }
        this.readiness = r;
        LOG.info("FelisLimbo: readiness endpoint on :" + port + Readiness.PATH + " (503 until the first tick)");

        // Limbo has no "started" event; one tick after onEnable the game socket is open.
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
        } catch (IOException e) {
            LOG.warning("FelisLimbo: " + e.getMessage());
            logLoginOff();
            return;
        }
        experience = ExperienceConfig.loadOrDefaults(ExperienceConfig.PATH, msg -> LOG.warning("FelisLimbo: " + msg));
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
                consoleUrl, timeoutMillis, !lobby.isEmpty(), System::currentTimeMillis, LOG);
        LOG.info("FelisLimbo: login flow ON — console=" + consoleUrl
                + ", lobby=" + (lobby.isEmpty() ? "none" : lobby) + ", window=" + (timeoutMillis / 1000) + "s");
    }

    private static void logLoginOff() {
        LOG.warning("FelisLimbo: login flow OFF (missing FELIS_API_BASE_URL/FELIS_SERVICE_TOKEN "
                + "or FELIS_ROOT_DOMAIN) — serving readiness only; this gate will NOT authenticate players");
    }

    @EventHandler
    public void onJoin(PlayerJoinEvent event) {
        flow.join(event.getPlayer().getUniqueId());
    }

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
