package best.lolicon.felis.limbo;

import best.lolicon.felis.link.Control;
import best.lolicon.felis.link.ControlFrame;
import best.lolicon.felis.link.FelisApiClient;
import best.lolicon.felis.link.LinkClient;
import best.lolicon.felis.link.LinkCode;
import best.lolicon.felis.link.LinkConfig;
import best.lolicon.felis.link.LinkConfigLoader;
import best.lolicon.felis.link.LinkException;

import com.loohp.limbo.events.EventHandler;
import com.loohp.limbo.events.Listener;
import com.loohp.limbo.events.player.PlayerJoinEvent;
import com.loohp.limbo.player.Player;
import com.loohp.limbo.plugins.LimboPlugin;

import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;

import net.kyori.adventure.inventory.Book;
import net.kyori.adventure.key.Key;
import net.kyori.adventure.text.Component;
import net.kyori.adventure.text.event.ClickEvent;
import net.kyori.adventure.text.format.NamedTextColor;

import java.io.File;
import java.io.IOException;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.util.Set;
import java.util.UUID;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.atomic.AtomicBoolean;
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
 * first server tick — the MinecraftServer CRD's HTTP readinessProbe follows that
 * true signal. Fail-closed: if the endpoint cannot bind, readiness never turns green
 * and the operator keeps the gate in Starting rather than advertising an unstarted
 * auth gate.
 *
 * <p><b>2. In-game login.</b> A player who reaches the limbo has been UUID-verified
 * upstream (Velocity online-mode) but is not yet linked to a web account. On join
 * the plugin, off the tick thread:
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
 * {@link #START_RETRY_WINDOW_MILLIS} before the player is turned away. Each retry
 * also sends a {@code LoginRelease}: the proxy keeps recent link confirmations for
 * exactly this case, so a player who was signed in minutes ago can still get
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
 * {@code FELIS_LOGIN_TIMEOUT_SECONDS} (default 600) bounds the login window. If the
 * link config or the root domain is absent the login flow stays OFF and the plugin
 * runs readiness-only — the same "load un-crippled" fail-safe the other Felis plugins
 * use — so a bare image still boots and serves readiness; production must supply the
 * config for the gate to authenticate.
 *
 * <p>CODE-ONLY in the Go repo: it compiles against the Limbo API + the shared link
 * core and is bundled into the login image (deploy/limbo), not built by the Go CI.
 */
public final class FelisLimboPlugin extends LimboPlugin implements Listener {

    private static final Logger LOG = Logger.getLogger("FelisLimbo");
    private static final String HEALTH_PATH = "/healthz";
    // Limbo deprecated the String channel overload; its Key overload sends key.toString(),
    // which for "felis:control" is the same channel string.
    private static final Key CONTROL_CHANNEL = Key.key(Control.CHANNEL);
    private static final int DEFAULT_PORT = 8080;

    // Poll cadence and window. 20 ticks ≈ 1s at Limbo's tick rate; polling once a
    // second is responsive without hammering felis-api. The default window (10 min)
    // matches the Bind Code TTL (linkCodeTTL in internal/api) — no point holding a
    // player past code expiry, and no point cutting them off while it is still valid.
    private static final long POLL_PERIOD_TICKS = 20L;
    private static final long DEFAULT_TIMEOUT_SECONDS = 600L;
    private static final long MIN_TIMEOUT_SECONDS = 30L;
    private static final long MAX_TIMEOUT_SECONDS = 3600L;

    // Backoff between retries, in ticks (≈1s, 2s, 4s, then every 8s): used both for a
    // felis-api outage at join and for re-sending the release until the player leaves.
    private static final long[] BACKOFF_TICKS = {20L, 40L, 80L, 160L};
    // How long a felis-api outage at join is retried before the player is turned away.
    static final long START_RETRY_WINDOW_MILLIS = 60_000L;
    // How long the release is re-sent after sign-in before giving up with a message.
    private static final long RELEASE_WINDOW_MILLIS = 120_000L;

    // ---- readiness state ----
    private final AtomicBoolean ready = new AtomicBoolean(false);
    private volatile HttpServer http;

    // ---- login state (populated only when the flow is configured) ----
    private volatile boolean loginEnabled;
    private volatile LinkClient linkClient;
    private volatile FelisApiClient apiClient;
    private volatile String consoleUrl;
    private volatile String lobbyServer;
    private volatile long timeoutMillis;

    // Per-player task ids (the link poll, a join retry, or the next release), so a
    // completed/abandoned login cancels its own timer rather than polling a departed
    // UUID forever.
    private final ConcurrentHashMap<UUID, Integer> pollTasks = new ConcurrentHashMap<>();
    // Players whose release loop is running; the async link poll can observe "linked"
    // twice before its cancellation lands, and the release must start once.
    private final Set<UUID> releasing = ConcurrentHashMap.newKeySet();

    @Override
    public void onEnable() {
        startReadiness();
        configureLogin();
        if (loginEnabled) {
            getServer().getEventsManager().registerEvents(this, this);
            LOG.info("FelisLimbo: login flow ON — console=" + consoleUrl
                    + ", lobby=" + lobbyServer + ", window=" + (timeoutMillis / 1000) + "s");
        } else {
            LOG.warning("FelisLimbo: login flow OFF (missing FELIS_API_BASE_URL/FELIS_SERVICE_TOKEN "
                    + "or FELIS_ROOT_DOMAIN) — serving readiness only; this gate will NOT authenticate players");
        }
    }

    @Override
    public void onDisable() {
        ready.set(false);
        HttpServer server = this.http;
        if (server != null) {
            server.stop(0);
            this.http = null;
        }
        // cancelTask(plugin) tears down every scheduled task this plugin owns.
        getServer().getScheduler().cancelTask(this);
        pollTasks.clear();
        releasing.clear();
    }

    // ---- readiness endpoint (unchanged behavior) ----

    private void startReadiness() {
        int port = healthPort();
        try {
            HttpServer server = HttpServer.create(new InetSocketAddress(port), 0);
            server.createContext(HEALTH_PATH, this::handleHealth);
            server.setExecutor(null); // default executor: this endpoint is trivial
            server.start();
            this.http = server;
            LOG.info("FelisLimbo: readiness endpoint on :" + port + HEALTH_PATH + " (503 until the first tick)");
        } catch (IOException e) {
            // Fail closed: no health server → never ready → pod stays NotReady.
            LOG.log(Level.SEVERE, "FelisLimbo: could not start readiness endpoint on :" + port
                    + " — the login gate will stay NotReady", e);
            return;
        }

        // onEnable runs just before Limbo opens its game socket and no "started"
        // event exists, so mark ready one tick later: by the time the scheduler runs
        // the task the server loop is ticking and the socket is open.
        getServer().getScheduler().runTaskLater(this, () -> {
            if (ready.compareAndSet(false, true)) {
                LOG.info("FelisLimbo: server started — readiness now 200");
            }
        }, 1L);
    }

    private void handleHealth(HttpExchange exchange) throws IOException {
        boolean up = ready.get();
        byte[] body = (up ? "ok" : "starting").getBytes(StandardCharsets.UTF_8);
        int status = up ? 200 : 503;
        exchange.getResponseHeaders().set("Content-Type", "text/plain; charset=utf-8");
        exchange.sendResponseHeaders(status, body.length);
        try (OutputStream os = exchange.getResponseBody()) {
            os.write(body);
        }
    }

    // ---- login configuration ----

    private void configureLogin() {
        LinkConfig cfg;
        try {
            File props = new File(getDataFolder(), "felis-link.properties");
            cfg = LinkConfigLoader.load(props.toPath());
        } catch (IOException e) {
            // Missing/half config: like the other Felis plugins, load un-crippled —
            // readiness stays up, the login flow just never turns on.
            LOG.warning("FelisLimbo: " + e.getMessage());
            loginEnabled = false;
            return;
        }

        // The console host the player links at. Prefer the resolved FELIS_PANEL_HOSTNAME
        // the provisioner bakes in (single source of truth — it honours a custom
        // panel_hostname); fall back to console.<root> only for an older operator whose
        // env predates it. With neither set there is no link to build, so login stays off.
        String panelHost = trimmed(System.getenv("FELIS_PANEL_HOSTNAME"));
        String rootDomain = trimmed(System.getenv("FELIS_ROOT_DOMAIN"));
        if (panelHost == null && rootDomain != null) {
            panelHost = "console." + rootDomain;
        }
        if (panelHost == null) {
            LOG.warning("FelisLimbo: neither FELIS_PANEL_HOSTNAME nor FELIS_ROOT_DOMAIN set — cannot build the console login link");
            loginEnabled = false;
            return;
        }

        this.consoleUrl = "https://" + panelHost;
        String lobby = trimmed(System.getenv("FELIS_LOBBY_SERVER"));
        this.lobbyServer = lobby != null ? lobby : "lobby";
        this.timeoutMillis = loginTimeoutSeconds() * 1000L;
        this.linkClient = new LinkClient(cfg);
        this.apiClient = new FelisApiClient(cfg);
        this.loginEnabled = true;
    }

    // ---- login flow ----

    @EventHandler
    public void onJoin(PlayerJoinEvent event) {
        Player player = event.getPlayer();
        UUID id = player.getUniqueId();
        releasing.remove(id); // a reconnect starts a fresh login
        long startedAt = System.currentTimeMillis();
        // Everything below touches the network; run it off the tick thread so a slow
        // felis-api never stalls the server loop. The player waits in the limbo world.
        getServer().getScheduler().runTaskAsync(this, () -> beginLogin(id, 0, startedAt));
    }

    private void beginLogin(UUID id, int attempt, long startedAt) {
        if (online(id) == null) {
            return; // left while a retry was pending
        }
        try {
            if (apiClient.isBlacklisted(id)) {
                disconnectOnMain(id, "该用户名已被回收保护 / This username is under reclaim protection. Contact staff.");
                return;
            }
            // Check registration before minting: an already-linked player needs no
            // bind code, so send them straight to the lobby instead of flashing a
            // useless code. Only unlinked players get one. The on-demand /link
            // command (proxy + lobby) stays the door to a fresh web session.
            if (apiClient.linkStatus(id)) {
                getServer().getScheduler().runTask(this, () -> startRelease(id));
                return;
            }
            LinkCode code = linkClient.requestCode(id);
            getServer().getScheduler().runTask(this, () -> presentAndPoll(id, code));
        } catch (LinkException e) {
            boolean outage = e.statusCode() == 0 || e.statusCode() >= 500;
            if (!outage || System.currentTimeMillis() - startedAt > START_RETRY_WINDOW_MILLIS) {
                // Fail closed: felis-api refused, or stayed unreachable for the whole
                // retry window. Refuse the connection rather than let them idle
                // unauthenticated.
                LOG.warning("FelisLimbo: login start failed for " + id + " after " + (attempt + 1)
                        + " attempt(s) — " + e.getMessage());
                disconnectOnMain(id, "登录服务暂不可用，请稍后重连 / Login service unavailable, please reconnect shortly.");
                return;
            }
            LOG.info("FelisLimbo: felis-api unavailable at login for " + id + " (attempt " + (attempt + 1)
                    + "): " + e.getMessage());
            getServer().getScheduler().runTask(this, () -> {
                Player player = online(id);
                if (player == null) {
                    return;
                }
                if (attempt == 0) {
                    player.sendMessage("§e[Felis] 登录服务繁忙，正在重试… / The login service is busy — retrying…");
                }
                sendRelease(player);
            });
            int taskId = getServer().getScheduler().runTaskLaterAsync(
                    this, () -> beginLogin(id, attempt + 1, startedAt), backoff(attempt));
            track(id, taskId);
        }
    }

    private void presentAndPoll(UUID id, LinkCode code) {
        Player player = getServer().getPlayer(id);
        if (player == null || !player.isValid()) {
            return; // player left during the async mint
        }

        // Prefer the panel URL the server minted with the code (it is the same
        // single source of truth felis-api holds); the env-built consoleUrl is the
        // fallback for an older API that does not emit panel_url yet.
        String url = code.panelUrl() != null ? code.panelUrl() : consoleUrl;
        try {
            player.openBook(loginBook(code, url));
        } catch (RuntimeException e) {
            // A client that refuses the book (rare) still gets the chat instructions
            // below, so a book failure is not fatal to the flow.
            LOG.fine("FelisLimbo: openBook failed for " + id + " — " + e.getMessage());
        }
        player.sendMessage("§e[Felis] 绑定码 / Code: §6" + code.code());
        player.sendMessage("§e[Felis] 用系统浏览器打开 §b" + url
                + " §e完成登录（勿用微信/QQ内置浏览器）。");
        player.sendMessage("§7Open " + url + " in your system browser (not WeChat/QQ) to finish.");

        long deadline = System.currentTimeMillis() + timeoutMillis;
        int taskId = getServer().getScheduler().runTaskTimerAsync(
                this, () -> pollOnce(id, deadline), POLL_PERIOD_TICKS, POLL_PERIOD_TICKS);
        track(id, taskId);
    }

    private void pollOnce(UUID id, long deadline) {
        Player player = getServer().getPlayer(id);
        if (player == null || !player.isValid()) {
            cancelPoll(id); // player left; stop polling their UUID
            return;
        }
        if (System.currentTimeMillis() > deadline) {
            cancelPoll(id);
            disconnectOnMain(id, "登录超时，请重连 / Login timed out. Please reconnect.");
            return;
        }
        try {
            if (apiClient.linkStatus(id)) {
                getServer().getScheduler().runTask(this, () -> startRelease(id));
            }
        } catch (LinkException e) {
            // A transient poll failure is not fatal — keep trying until the deadline.
            LOG.fine("FelisLimbo: link status poll failed for " + id + " — " + e.getMessage());
        }
    }

    // startRelease runs on the main thread once the player is known to be linked: stop
    // the link poll, say so once, and start asking the proxy for the lobby.
    private void startRelease(UUID id) {
        Player player = online(id);
        if (player == null || !releasing.add(id)) {
            return;
        }
        cancelPoll(id);
        player.sendMessage("§a[Felis] 登录成功，正在进入大厅… / Signed in — sending you to the lobby…");
        releaseAttempt(id, 0, System.currentTimeMillis() + RELEASE_WINDOW_MILLIS);
    }

    // releaseAttempt sends one LoginRelease and schedules the next. The proxy re-checks
    // the link before it moves the player, and a transient failure there only denies
    // that one attempt, so the gate keeps asking (with backoff, silently) until the
    // player is gone or the window closes.
    private void releaseAttempt(UUID id, int attempt, long deadline) {
        Player player = online(id);
        if (player == null) {
            releasing.remove(id);
            cancelPoll(id);
            return;
        }
        if (System.currentTimeMillis() > deadline) {
            releasing.remove(id);
            LOG.warning("FelisLimbo: " + id + " was not released to the lobby within "
                    + (RELEASE_WINDOW_MILLIS / 1000) + "s");
            disconnectOnMain(id, "进入大厅失败，请重连 / Could not reach the lobby. Please reconnect.");
            return;
        }
        sendRelease(player);
        int taskId = getServer().getScheduler().runTaskLater(
                this, () -> releaseAttempt(id, attempt + 1, deadline), backoff(attempt));
        track(id, taskId);
    }

    private void sendRelease(Player player) {
        try {
            player.sendPluginMessage(CONTROL_CHANNEL, Control.encode(ControlFrame.loginRelease(player.getName())));
        } catch (IOException | RuntimeException e) {
            // The next attempt sends again; the window bounds how long we keep trying.
            LOG.warning("FelisLimbo: could not send the lobby release for " + player.getUniqueId()
                    + " — " + e.getMessage());
        }
    }

    private Player online(UUID id) {
        Player player = getServer().getPlayer(id);
        return player != null && player.isValid() ? player : null;
    }

    // track records the player's current task, cancelling the one it replaces.
    private void track(UUID id, int taskId) {
        Integer previous = pollTasks.put(id, taskId);
        if (previous != null && previous != taskId) {
            getServer().getScheduler().cancelTask(previous);
        }
    }

    private static long backoff(int attempt) {
        return BACKOFF_TICKS[Math.min(attempt, BACKOFF_TICKS.length - 1)];
    }

    private void cancelPoll(UUID id) {
        Integer taskId = pollTasks.remove(id);
        if (taskId != null) {
            getServer().getScheduler().cancelTask(taskId);
        }
    }

    private void disconnectOnMain(UUID id, String message) {
        getServer().getScheduler().runTask(this, () -> {
            cancelPoll(id);
            Player player = getServer().getPlayer(id);
            if (player != null && player.isValid()) {
                player.disconnect(message);
            }
        });
    }

    // ---- rendering / wire ----

    private Book loginBook(LinkCode code, String url) {
        Component page = Component.text("Felis 登录 / Login\n\n")
                .append(Component.text("绑定码 / Code:\n"))
                .append(Component.text(code.code() + "\n\n").color(NamedTextColor.GOLD))
                .append(Component.text("▶ 点此打开登录页\n▶ Open login page\n")
                        .color(NamedTextColor.AQUA)
                        .clickEvent(ClickEvent.openUrl(url)))
                .append(Component.text("\n在系统浏览器中完成。\nUse your SYSTEM browser —\nnot WeChat / QQ (passkey\nwon't work there).")
                        .color(NamedTextColor.GRAY));
        return Book.book(
                Component.text("Felis Login"),
                Component.text("Felis"),
                page);
    }

    // ---- env helpers ----

    private static int healthPort() {
        String raw = System.getenv("FELIS_HEALTH_PORT");
        if (raw != null && !raw.isBlank()) {
            try {
                int p = Integer.parseInt(raw.trim());
                if (p > 0 && p < 65536) {
                    return p;
                }
            } catch (NumberFormatException ignored) {
                // fall through to the default
            }
        }
        return DEFAULT_PORT;
    }

    private static long loginTimeoutSeconds() {
        String raw = System.getenv("FELIS_LOGIN_TIMEOUT_SECONDS");
        if (raw != null && !raw.isBlank()) {
            try {
                long s = Long.parseLong(raw.trim());
                if (s < MIN_TIMEOUT_SECONDS) {
                    return MIN_TIMEOUT_SECONDS;
                }
                if (s > MAX_TIMEOUT_SECONDS) {
                    return MAX_TIMEOUT_SECONDS;
                }
                return s;
            } catch (NumberFormatException ignored) {
                // fall through to the default
            }
        }
        return DEFAULT_TIMEOUT_SECONDS;
    }

    private static String trimmed(String raw) {
        if (raw == null) {
            return null;
        }
        String t = raw.trim();
        return t.isEmpty() ? null : t;
    }
}
