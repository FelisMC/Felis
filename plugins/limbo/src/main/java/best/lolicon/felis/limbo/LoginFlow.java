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

/**
 * LoginFlow is the login gate's per-player flow (see {@link FelisLimboPlugin}): the
 * blacklist check, the bind code, the link poll and the lobby release, each with its
 * retry and its deadline. It holds no Limbo type; the plugin hands it a {@link Gate}
 * (the scheduler and the online players) and a {@link Seat} per player, which is what
 * lets LoginFlowTest run the whole flow on a virtual clock against a stub felis-api.
 *
 * <p>Threading follows Limbo's: every felis-api call runs in an async task, and every
 * touch of a player (chat, book, plugin message, disconnect) in a sync one.
 */
final class LoginFlow {

    /** Gate is the Limbo server as the flow sees it. Delays are in ticks (≈50 ms). */
    interface Gate {
        /** player is the online player with this UUID, or null once they have left. */
        Seat player(UUID id);

        int runSync(Runnable task);

        int runAsync(Runnable task);

        int runLater(Runnable task, long delayTicks);

        int runLaterAsync(Runnable task, long delayTicks);

        int runTimerAsync(Runnable task, long delayTicks, long periodTicks);

        void cancel(int taskId);
    }

    /** Seat is one player waiting in the gate. */
    interface Seat {
        String name();

        /** chat sends one line, with § colour codes. */
        void chat(String line);

        /** showCode opens the login book. It may throw for a client that refuses it. */
        void showCode(LinkCode code, String url);

        void sendControl(byte[] frame) throws IOException;

        void disconnect(String reason);
    }

    // Poll cadence: 20 ticks ≈ 1 s is responsive without hammering felis-api.
    static final long POLL_PERIOD_TICKS = 20L;
    // Backoff between retries, in ticks (≈1s, 2s, 4s, then every 8s): used both for a
    // felis-api outage at join and for re-sending the release until the player leaves.
    static final long[] BACKOFF_TICKS = {20L, 40L, 80L, 160L};
    // How long a felis-api outage at join is retried before the player is turned away.
    static final long START_RETRY_WINDOW_MILLIS = 60_000L;
    // How long the release is re-sent after sign-in before giving up with a message.
    static final long RELEASE_WINDOW_MILLIS = 120_000L;
    // How often a link-status outage is summarized while it lasts.
    static final long POLL_OUTAGE_REPEAT_MILLIS = 5 * 60_000L;

    static final String BLACKLISTED =
            "该用户名已被回收保护 / This username is under reclaim protection. Contact staff.";
    static final String UNAVAILABLE =
            "登录服务暂不可用，请稍后重连 / Login service unavailable, please reconnect shortly.";
    static final String TIMED_OUT = "登录超时，请重连 / Login timed out. Please reconnect.";
    static final String NO_LOBBY = "进入大厅失败，请重连 / Could not reach the lobby. Please reconnect.";
    static final String BUSY = "§e[Felis] 登录服务繁忙，正在重试… / The login service is busy — retrying…";
    static final String SIGNED_IN = "§a[Felis] 登录成功，正在进入大厅… / Signed in — sending you to the lobby…";

    private final Gate gate;
    private final FelisApiClient api;
    private final LinkClient link;
    private final String consoleUrl;
    private final long timeoutMillis;
    private final LongSupplier clock;
    private final Logger log;

    // Per-player task ids (the link poll, a join retry, or the next release), so a
    // completed/abandoned login cancels its own timer rather than polling a departed
    // UUID forever.
    private final ConcurrentHashMap<UUID, Integer> tasks = new ConcurrentHashMap<>();
    // Players whose release loop is running; the async link poll can observe "linked"
    // twice before its cancellation lands, and the release must start once.
    private final Set<UUID> releasing = ConcurrentHashMap.newKeySet();
    // One tracker for every player's link poll: the polls share felis-api, so an outage
    // is one event, reported when it starts, every few minutes while it lasts, and when
    // it ends, rather than once a second per waiting player.
    private final OutageTracker pollOutage;

    LoginFlow(Gate gate, FelisApiClient api, LinkClient link, String consoleUrl, long timeoutMillis,
              LongSupplier clock, Logger log) {
        this.gate = gate;
        this.api = api;
        this.link = link;
        this.consoleUrl = consoleUrl;
        this.timeoutMillis = timeoutMillis;
        this.clock = clock;
        this.log = log;
        this.pollOutage = new OutageTracker(POLL_OUTAGE_REPEAT_MILLIS, clock);
    }

    /** join starts a fresh login for a player who just connected. */
    void join(UUID id) {
        releasing.remove(id); // a reconnect starts a fresh login
        long startedAt = clock.getAsLong();
        // Everything below touches the network; run it off the tick thread so a slow
        // felis-api never stalls the server loop. The player waits in the limbo world.
        gate.runAsync(() -> begin(id, 0, startedAt));
    }

    /** clear forgets every player; the plugin cancels the tasks themselves. */
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
            // Check registration before minting: an already-linked player needs no
            // bind code, so send them straight to the lobby instead of flashing a
            // useless code. Only unlinked players get one. The on-demand /link
            // command (proxy + lobby) stays the door to a fresh web session.
            if (api.linkStatus(id)) {
                gate.runSync(() -> startRelease(id));
                return;
            }
            LinkCode code = link.requestCode(id);
            gate.runSync(() -> presentAndPoll(id, code));
        } catch (LinkException e) {
            boolean outage = e.statusCode() == 0 || e.statusCode() >= 500;
            if (!outage || clock.getAsLong() - startedAt > START_RETRY_WINDOW_MILLIS) {
                // Fail closed: felis-api refused, or stayed unreachable for the whole
                // retry window. Refuse the connection rather than let them idle
                // unauthenticated.
                log.warning("FelisLimbo: login start failed for " + id + " after " + (attempt + 1)
                        + " attempt(s) — " + e.getMessage());
                disconnect(id, UNAVAILABLE);
                return;
            }
            log.info("FelisLimbo: felis-api unavailable at login for " + id + " (attempt " + (attempt + 1)
                    + "): " + e.getMessage());
            // Each retry also asks for the lobby: the proxy keeps recent link
            // confirmations for exactly this case, so a player signed in minutes ago
            // gets through a felis-api restart, and anyone else is refused there.
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
        // Prefer the panel URL the server minted with the code (it is the same
        // single source of truth felis-api holds); the env-built consoleUrl is the
        // fallback for an older API that does not emit panel_url yet.
        String url = code.panelUrl() != null ? code.panelUrl() : consoleUrl;
        try {
            seat.showCode(code, url);
        } catch (RuntimeException e) {
            // A client that refuses the book (rare) still gets the chat instructions
            // below, so a book failure is not fatal to the flow.
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
            cancel(id); // player left; stop polling their UUID
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
            // A transient poll failure is not fatal — keep trying until the deadline.
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

    // startRelease runs on the main thread once the player is known to be linked: stop
    // the link poll, say so once, and start asking the proxy for the lobby.
    private void startRelease(UUID id) {
        Seat seat = gate.player(id);
        if (seat == null || !releasing.add(id)) {
            return;
        }
        cancel(id);
        seat.chat(SIGNED_IN);
        releaseAttempt(id, 0, clock.getAsLong() + RELEASE_WINDOW_MILLIS);
    }

    // releaseAttempt sends one LoginRelease and schedules the next. The proxy re-checks
    // the link before it moves the player, and a transient failure there only denies
    // that one attempt, so the gate keeps asking (with backoff, silently) until the
    // player is gone or the window closes.
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
            // The next attempt sends again; the window bounds how long we keep trying.
            log.warning("FelisLimbo: could not send the lobby release for " + id + " — " + e.getMessage());
        }
    }

    // track records the player's current task, cancelling the one it replaces.
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
