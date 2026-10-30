package best.lolicon.felis.limbo;

import best.lolicon.felis.link.Control;
import best.lolicon.felis.link.ControlFrame;
import best.lolicon.felis.link.FelisApiClient;
import best.lolicon.felis.link.LinkClient;
import best.lolicon.felis.link.LinkCode;
import best.lolicon.felis.link.LinkException;
import best.lolicon.felis.link.OutageTracker;

import java.io.IOException;
import java.util.Set;
import java.util.UUID;
import java.util.concurrent.ConcurrentHashMap;
import java.util.function.LongSupplier;
import java.util.logging.Logger;

// Run API calls asynchronously and player access on the server thread.
final class LoginFlow {

    /** Delays are in ticks (about 50 ms). */
    interface Gate {
        /** Returns the online player with this UUID, or null once they have left. */
        Seat player(UUID id);

        int runSync(Runnable task);

        int runAsync(Runnable task);

        int runLater(Runnable task, long delayTicks);

        int runLaterAsync(Runnable task, long delayTicks);

        int runTimerAsync(Runnable task, long delayTicks, long periodTicks);

        void cancel(int taskId);
    }

    interface Seat {
        String name();

        /** Sends one line with § colour codes; a web address in it is clickable. */
        void chat(String line);

        /** Opens the login book; may throw for a client that refuses it. */
        void showCode(LinkCode code, String url);

        void sendControl(byte[] frame) throws IOException;

        void disconnect(String reason);
    }

    static final long POLL_PERIOD_TICKS = 20L;
    // Retry delays in ticks (1 s, 2 s, 4 s, then 8 s) for join retries and release re-sends.
    static final long[] BACKOFF_TICKS = {20L, 40L, 80L, 160L};
    static final long START_RETRY_WINDOW_MILLIS = 60_000L; // felis-api outage retry window at join
    static final long RELEASE_WINDOW_MILLIS = 120_000L; // how long the release is re-sent after sign-in
    static final long POLL_OUTAGE_REPEAT_MILLIS = 5 * 60_000L; // link-status outage summary interval
    static final String BLACKLISTED =
            "该用户名已被回收保护 / This username is under reclaim protection. Contact staff.";
    static final String UNAVAILABLE =
            "登录服务暂不可用，请稍后重连 / Login service unavailable, please reconnect shortly.";
    static final String TIMED_OUT = "登录超时，请重连 / Login timed out. Please reconnect.";
    static final String NO_LOBBY = "进入大厅失败，请重连 / Could not reach the lobby. Please reconnect.";
    static final String BUSY = "§e[Felis] 登录服务繁忙，正在重试… / The login service is busy — retrying…";
    static final String SIGNED_IN = "§a[Felis] 登录成功，正在进入大厅… / Signed in — sending you to the lobby…";
    static final String SIGNED_IN_NO_LOBBY = "§a[Felis] 登录成功，正在进入服务器… / Signed in — sending you to your server…";

    private final Gate gate;
    private final FelisApiClient api;
    private final LinkClient link;
    private final String consoleUrl;
    private final long timeoutMillis;
    private final boolean hasLobby;
    private final LongSupplier clock;
    private final Logger log;

    private final ConcurrentHashMap<UUID, Integer> tasks = new ConcurrentHashMap<>(); // current timer task per player
    // The async link poll can see "linked" twice before cancellation lands; the release must start once.
    private final Set<UUID> releasing = ConcurrentHashMap.newKeySet();
    private final OutageTracker pollOutage; // shared by all polls: one felis-api outage is one event

    LoginFlow(Gate gate, FelisApiClient api, LinkClient link, String consoleUrl, long timeoutMillis,
              boolean hasLobby, LongSupplier clock, Logger log) {
        this.gate = gate;
        this.api = api;
        this.link = link;
        this.consoleUrl = consoleUrl;
        this.timeoutMillis = timeoutMillis;
        this.hasLobby = hasLobby;
        this.clock = clock;
        this.log = log;
        this.pollOutage = new OutageTracker(POLL_OUTAGE_REPEAT_MILLIS, clock);
    }

    void join(UUID id) {
        releasing.remove(id); // a reconnect starts a fresh login
        long startedAt = clock.getAsLong();
        gate.runAsync(() -> begin(id, 0, startedAt));
    }

    /** Forgets every player; the plugin cancels the scheduled tasks. */
    void clear() {
        tasks.clear();
        releasing.clear();
    }

    private void begin(UUID id, int attempt, long startedAt) {
        if (gate.player(id) == null) {
            return; // left while a retry was pending
        }
        try {
            if (api.isBlacklisted(id)) {
                disconnect(id, BLACKLISTED);
                return;
            }
            if (!api.entryPolicy().requireAccountLink()) {
                // The proxy owns routing and startup progress; this space is only a waiting room.
                return;
            }
            // An already-linked player needs no bind code.
            if (api.linkStatus(id)) {
                gate.runSync(() -> startRelease(id));
                return;
            }
            LinkCode code = link.requestCode(id);
            gate.runSync(() -> presentAndPoll(id, code));
        } catch (LinkException e) {
            boolean outage = e.statusCode() == 0 || e.statusCode() >= 500;
            if (!outage || clock.getAsLong() - startedAt > START_RETRY_WINDOW_MILLIS) {
                log.warning("FelisLimbo: login start failed for " + id + " after " + (attempt + 1)
                        + " attempt(s) — " + e.getMessage());
                disconnect(id, UNAVAILABLE);
                return;
            }
            log.info("FelisLimbo: felis-api unavailable at login for " + id + " (attempt " + (attempt + 1)
                    + "): " + e.getMessage());
            // The proxy keeps recent link confirmations, so a recently signed-in player gets through an outage.
            gate.runSync(() -> {
                Seat seat = gate.player(id);
                if (seat == null) {
                    return;
                }
                if (attempt == 0) {
                    seat.chat(BUSY);
                }
                sendRelease(id, seat);
            });
            track(id, gate.runLaterAsync(() -> begin(id, attempt + 1, startedAt), backoff(attempt)));
        }
    }

    private void presentAndPoll(UUID id, LinkCode code) {
        Seat seat = gate.player(id);
        if (seat == null) {
            return; // player left during the async mint
        }
        // Falls back to the env-built consoleUrl when the API emits no panel_url.
        String url = code.panelUrl() != null ? code.panelUrl() : consoleUrl;
        try {
            seat.showCode(code, url);
        } catch (RuntimeException e) {
            log.warning("FelisLimbo: openBook failed for " + id + " — " + e.getMessage()
                    + "; the code is still sent in chat");
        }
        seat.chat("§e[Felis] 绑定码 / Code: §6" + code.code());
        seat.chat("§e[Felis] 用系统浏览器打开 §b" + url + " §e完成登录（勿用微信/QQ内置浏览器）。");
        seat.chat("§7Open " + url + " in your system browser (not WeChat/QQ) to finish.");

        long deadline = clock.getAsLong() + timeoutMillis;
        track(id, gate.runTimerAsync(() -> pollOnce(id, deadline), POLL_PERIOD_TICKS, POLL_PERIOD_TICKS));
    }

    private void pollOnce(UUID id, long deadline) {
        if (gate.player(id) == null) {
            cancel(id); // player left
            return;
        }
        if (clock.getAsLong() > deadline) {
            cancel(id);
            disconnect(id, TIMED_OUT);
            return;
        }
        boolean linked;
        try {
            linked = api.linkStatus(id);
        } catch (LinkException e) {
            OutageTracker.Report report = pollOutage.failure();
            if (report == OutageTracker.Report.DOWN) {
                log.warning("FelisLimbo: link status poll failed for " + id + " — " + e.getMessage()
                        + "; players keep waiting, and repeats are summarized every "
                        + POLL_OUTAGE_REPEAT_MILLIS / 60_000L + " min until it recovers");
            } else if (report == OutageTracker.Report.STILL_DOWN) {
                log.warning("FelisLimbo: link status polls still failing: " + pollOutage.failures()
                        + " failed polls over " + pollOutage.downForMillis() / 1000 + " s, last — "
                        + e.getMessage());
            }
            return;
        }
        if (pollOutage.success() == OutageTracker.Report.RECOVERED) {
            log.info("FelisLimbo: link status polls recovered after " + pollOutage.lastOutageFailures()
                    + " failed polls over " + pollOutage.lastOutageMillis() / 1000 + " s");
        }
        if (linked) {
            gate.runSync(() -> startRelease(id));
        }
    }

    // Runs on the main thread once the player is linked: stops the link poll and starts the release.
    private void startRelease(UUID id) {
        Seat seat = gate.player(id);
        if (seat == null || !releasing.add(id)) {
            return;
        }
        cancel(id);
        seat.chat(hasLobby ? SIGNED_IN : SIGNED_IN_NO_LOBBY);
        // Without a lobby the proxy holds the player through the server's start and owns that timeout.
        releaseAttempt(id, 0, hasLobby ? clock.getAsLong() + RELEASE_WINDOW_MILLIS : Long.MAX_VALUE);
    }

    // The proxy re-checks the link on each LoginRelease, so a denied attempt is retried until the window closes.
    private void releaseAttempt(UUID id, int attempt, long deadline) {
        Seat seat = gate.player(id);
        if (seat == null) {
            releasing.remove(id);
            cancel(id);
            return;
        }
        if (clock.getAsLong() > deadline) {
            releasing.remove(id);
            log.warning("FelisLimbo: " + id + " was not released to the lobby within "
                    + (RELEASE_WINDOW_MILLIS / 1000) + "s");
            disconnect(id, NO_LOBBY);
            return;
        }
        sendRelease(id, seat);
        track(id, gate.runLater(() -> releaseAttempt(id, attempt + 1, deadline), backoff(attempt)));
    }

    private void sendRelease(UUID id, Seat seat) {
        try {
            seat.sendControl(Control.encode(ControlFrame.loginRelease(seat.name())));
        } catch (IOException | RuntimeException e) {
            log.warning("FelisLimbo: could not send the release for " + id + " — " + e.getMessage());
        }
    }

    private void track(UUID id, int taskId) {
        Integer previous = tasks.put(id, taskId);
        if (previous != null && previous != taskId) {
            gate.cancel(previous);
        }
    }

    private static long backoff(int attempt) {
        return BACKOFF_TICKS[Math.min(attempt, BACKOFF_TICKS.length - 1)];
    }

    private void cancel(UUID id) {
        Integer taskId = tasks.remove(id);
        if (taskId != null) {
            gate.cancel(taskId);
        }
    }

    private void disconnect(UUID id, String reason) {
        gate.runSync(() -> {
            cancel(id);
            Seat seat = gate.player(id);
            if (seat != null) {
                seat.disconnect(reason);
            }
        });
    }
}
