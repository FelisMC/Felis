package best.lolicon.felis.limbo;

import best.lolicon.felis.link.Control;
import best.lolicon.felis.link.ControlFrame;
import best.lolicon.felis.link.FelisApiClient;
import best.lolicon.felis.link.LinkClient;
import best.lolicon.felis.link.LinkCode;
import best.lolicon.felis.link.LinkConfig;

import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;

import net.kyori.adventure.inventory.Book;
import net.kyori.adventure.text.Component;
import net.kyori.adventure.text.TextComponent;
import net.kyori.adventure.text.event.ClickEvent;
import net.kyori.adventure.text.format.TextDecoration;

import java.io.IOException;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.UUID;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * LoginFlowTest runs the login gate's whole flow on a virtual clock: a fake Limbo
 * (scheduler and players) around the real LoginFlow, and the real FelisApiClient and
 * LinkClient against a stub felis-api. It checks what a player at the front door
 * gets in each case: a linked player goes straight to the lobby, an unlinked one gets
 * the code and is released once the web console links them, a barred one is turned
 * away, a felis-api outage is retried for its window (asking the proxy for the lobby
 * on each try) and a refusal is not, the login and release windows end in the right
 * disconnect, a player who leaves stops every timer, and a reconnect starts afresh.
 * Then the pieces around it: the readiness endpoint, the env parsing, the book.
 *
 * <p>Run: {@code ./gradlew -PlimboVersion=<LIMBO_VERSION> loginTest} in plugins/limbo
 * (plugins/test.sh does).
 */
public final class LoginFlowTest {

    private static final String CONSOLE = "https://console.example.test";
    private static final String CODE = "K7Q2-9XPM";
    // The flow logs its retries and refusals; the test's own output is the checks. A
    // static field keeps the logger (and so its level) from being collected.
    private static final Logger QUIET = Logger.getLogger("LoginFlowTest");
    private static int checks;

    public static void main(String[] args) throws Exception {
        QUIET.setLevel(Level.OFF);
        Stub stub = new Stub();
        try {
            linkedPlayerGoesStraightToTheLobby(stub);
            unlinkedPlayerGetsTheCodeAndIsReleasedOnceLinked(stub);
            codeWithoutPanelUrlPointsAtTheConsole(stub);
            refusedBookStillLeavesTheCodeInChat(stub);
            barredPlayerIsTurnedAway(stub);
            loginWindowEndsInATimeout(stub);
            outageAtJoinIsRetriedThenGivesUp(stub);
            outageAtJoinThatRecoversCarriesOn(stub);
            refusalAtJoinIsNotRetried(stub);
            pollFailuresAreNotFatal(stub);
            leavingStopsEveryTimer(stub);
            releaseWindowEndsInADisconnect(stub);
            reconnectStartsAFreshRelease(stub);
            laggingMainThreadReleasesOnce(stub);
            reconnectDuringAnOutageRunsOneRetryLoop(stub);
        } finally {
            stub.stop();
        }
        readiness();
        gateConfig();
        loginBook();
        System.out.println("LoginFlowTest OK (" + checks + " checks)");
    }

    // ---- the flow ----

    private static void linkedPlayerGoesStraightToTheLobby(Stub stub) {
        Rig rig = new Rig(stub);
        UUID id = rig.stub.player(false, true);
        Seat seat = rig.join(id, "Steve");
        assertEq("linked: signed in, no code", List.of(LoginFlow.SIGNED_IN), seat.chat);
        assertEq("linked: nothing minted", 0, stub.mints.get());
        assertEq("linked: one release right away", 1, seat.releases.size());
        assertEq("the release names the player", ControlFrame.loginRelease("Steve"), seat.releases.get(0));
        // Re-sent with backoff (1 s, 2 s, 4 s, then every 8 s) while the player is here.
        long[] gaps = {20, 40, 80, 160, 160};
        for (int i = 0; i < gaps.length; i++) {
            rig.gate.advance(gaps[i] - 1);
            assertEq("no release before the " + gaps[i] + "-tick gap", i + 1, seat.releases.size());
            rig.gate.advance(1);
            assertEq("release after the " + gaps[i] + "-tick gap", i + 2, seat.releases.size());
        }
        rig.gate.leave(id);
        rig.gate.advance(400);
        assertEq("no release after the player left", 6, seat.releases.size());
        assertEq("no timer left behind", 0, rig.gate.pending());
        assertEq("linked: never disconnected", null, seat.disconnected);
    }

    private static void unlinkedPlayerGetsTheCodeAndIsReleasedOnceLinked(Stub stub) {
        Rig rig = new Rig(stub);
        UUID id = stub.player(false, false);
        stub.panelUrl = "https://panel.example.test/link";
        Seat seat = rig.join(id, "Alex");
        assertEq("unlinked: the book carries the code and the minted panel URL",
                List.of(CODE + " " + stub.panelUrl), seat.books);
        assertTrue("chat carries the code", seat.chat.get(0).contains(CODE));
        assertTrue("chat names the panel URL", seat.chat.get(1).contains(stub.panelUrl) && seat.chat.get(2).contains(stub.panelUrl));
        // The client makes nothing in server chat clickable by itself: a player who
        // closed the book had to type the URL out.
        for (int i = 1; i <= 2; i++) {
            List<ClickEvent> clicks = new ArrayList<>();
            collectClicks(LoginBook.chatLine(seat.chat.get(i)), clicks);
            assertEq("chat line " + i + " opens the panel URL", List.of(ClickEvent.openUrl(stub.panelUrl)), clicks);
        }
        assertEq("no release before the link", 0, seat.releases.size());

        int before = stub.hits("/link/status/");
        rig.gate.advance(20 * 5);
        assertEq("polled once a second", before + 5, stub.hits("/link/status/"));

        stub.linked.put(id, true);
        rig.gate.advance(20);
        assertEq("released once linked", 1, seat.releases.size());
        assertEq("told they are through", LoginFlow.SIGNED_IN, seat.chat.get(seat.chat.size() - 1));
        int after = stub.hits("/link/status/");
        rig.gate.advance(20 * 10);
        assertEq("the poll stops after the release starts", after, stub.hits("/link/status/"));
        assertTrue("the release keeps going instead", seat.releases.size() > 1);
        stub.panelUrl = null;
    }

    private static void codeWithoutPanelUrlPointsAtTheConsole(Stub stub) {
        Rig rig = new Rig(stub);
        Seat seat = rig.join(stub.player(false, false), "Alex");
        assertEq("an older felis-api's code: the book falls back to the console", List.of(CODE + " " + CONSOLE), seat.books);
        assertTrue("... and so does the chat", seat.chat.get(1).contains(CONSOLE));
    }

    private static void refusedBookStillLeavesTheCodeInChat(Stub stub) {
        Rig rig = new Rig(stub);
        UUID id = stub.player(false, false);
        Seat seat = rig.gate.seat(id, "Alex");
        seat.refusesBook = true;
        rig.flow.join(id);
        rig.gate.advance(0);
        assertTrue("a refused book still leaves the code in chat", seat.chat.get(0).contains(CODE));
        stub.linked.put(id, true);
        rig.gate.advance(20);
        assertEq("... and the login carries on", 1, seat.releases.size());
    }

    private static void barredPlayerIsTurnedAway(Stub stub) {
        Rig rig = new Rig(stub);
        int mints = stub.mints.get();
        UUID id = stub.player(true, true);
        Seat seat = rig.join(id, "Squatter");
        assertEq("barred: disconnected with the reclaim notice", LoginFlow.BLACKLISTED, seat.disconnected);
        assertEq("barred: nothing minted", mints, stub.mints.get());
        assertEq("barred: no release even though linked", 0, seat.releases.size());
        assertEq("barred: no timer left", 0, rig.gate.pending());
    }

    private static void loginWindowEndsInATimeout(Stub stub) {
        Rig rig = new Rig(stub, 30_000L);
        Seat seat = rig.join(stub.player(false, false), "Slow");
        rig.gate.advance(600); // exactly 30 s after the code
        assertEq("still waiting at the end of the window", null, seat.disconnected);
        rig.gate.advance(20);
        assertEq("timed out right after it", LoginFlow.TIMED_OUT, seat.disconnected);
        int polls = stub.hits("/link/status/");
        rig.gate.advance(200);
        assertEq("no poll after the timeout", polls, stub.hits("/link/status/"));
        assertEq("no timer left", 0, rig.gate.pending());
    }

    private static void outageAtJoinIsRetriedThenGivesUp(Stub stub) {
        Rig rig = new Rig(stub);
        UUID id = stub.player(false, false);
        stub.failWith = 500;
        Seat seat = rig.join(id, "Early");
        assertEq("the player hears once that it is retrying", List.of(LoginFlow.BUSY), seat.chat);
        assertEq("each try asks the proxy for the lobby", 1, seat.releases.size());
        rig.gate.advance(1180); // 59 s
        assertEq("still retrying inside the minute", null, seat.disconnected);
        int tries = seat.releases.size();
        assertTrue("retried with backoff, not every tick: " + tries, tries >= 8 && tries <= 12);
        assertEq("... and says 'retrying' only once", List.of(LoginFlow.BUSY), seat.chat);
        rig.gate.advance(160);
        assertEq("turned away once the minute is over", LoginFlow.UNAVAILABLE, seat.disconnected);
        rig.gate.advance(400);
        assertEq("no retry after giving up", 0, rig.gate.pending());
        stub.failWith = 0;
    }

    private static void outageAtJoinThatRecoversCarriesOn(Stub stub) {
        Rig rig = new Rig(stub);
        UUID id = stub.player(false, false);
        stub.failWith = 503;
        Seat seat = rig.join(id, "Early");
        rig.gate.advance(20 + 40 - 1);
        assertEq("still down: no code yet", List.of(), seat.books);
        stub.failWith = 0;
        rig.gate.advance(1);
        assertEq("felis-api back: the code comes", List.of(CODE + " " + CONSOLE), seat.books);
        assertEq("... and nobody was turned away", null, seat.disconnected);
    }

    private static void refusalAtJoinIsNotRetried(Stub stub) {
        Rig rig = new Rig(stub);
        int mints = stub.mints.get();
        stub.mintStatus = 429;
        Seat seat = rig.join(stub.player(false, false), "Spammy");
        assertEq("a refused mint turns the player away at once", LoginFlow.UNAVAILABLE, seat.disconnected);
        assertEq("... after one try", mints + 1, stub.mints.get());
        assertEq("... without asking for the lobby", 0, seat.releases.size());
        stub.mintStatus = 201;
    }

    private static void pollFailuresAreNotFatal(Stub stub) {
        Rig rig = new Rig(stub);
        UUID id = stub.player(false, false);
        Seat seat = rig.join(id, "Patient");
        stub.failWith = 500;
        rig.gate.advance(20 * 30);
        assertEq("half a minute of failed polls: still waiting", null, seat.disconnected);
        stub.failWith = 0;
        stub.linked.put(id, true);
        rig.gate.advance(20);
        assertEq("released once felis-api answers again", 1, seat.releases.size());
    }

    private static void leavingStopsEveryTimer(Stub stub) {
        Rig rig = new Rig(stub);
        UUID id = stub.player(false, false);
        rig.join(id, "Gone");
        rig.gate.leave(id);
        rig.gate.advance(20);
        int polls = stub.hits("/link/status/");
        rig.gate.advance(20 * 20);
        assertEq("no poll for a player who left", polls, stub.hits("/link/status/"));
        assertEq("no timer left", 0, rig.gate.pending());
    }

    private static void releaseWindowEndsInADisconnect(Stub stub) {
        Rig rig = new Rig(stub);
        Seat seat = rig.join(stub.player(false, true), "Stuck");
        rig.gate.advance(2400 - 1); // just short of two minutes
        assertEq("still being released inside the window", null, seat.disconnected);
        rig.gate.advance(160);
        assertEq("told the lobby could not be reached", LoginFlow.NO_LOBBY, seat.disconnected);
        assertEq("no timer left", 0, rig.gate.pending());
    }

    private static void reconnectStartsAFreshRelease(Stub stub) {
        Rig rig = new Rig(stub);
        UUID id = stub.player(false, true);
        rig.join(id, "Back");
        rig.gate.leave(id);
        Seat again = rig.join(id, "Back"); // back before the release loop noticed
        assertEq("the reconnect is told it is through again", List.of(LoginFlow.SIGNED_IN), again.chat);
        assertEq("... and released", 1, again.releases.size());
        rig.gate.advance(20);
        assertEq("one release loop, not two", 2, again.releases.size());
    }

    private static void laggingMainThreadReleasesOnce(Stub stub) {
        Rig rig = new Rig(stub);
        UUID id = stub.player(false, false);
        Seat seat = rig.join(id, "Busy");
        // The main thread falls two polls behind: the async poll sees "linked" at 20,
        // 40 and 60 before the first queued release start runs at 65.
        rig.gate.syncLag = 45;
        stub.linked.put(id, true);
        rig.gate.advance(120);
        long signedIn = seat.chat.stream().filter(LoginFlow.SIGNED_IN::equals).count();
        assertEq("a lagging main thread still says 'signed in' once", 1L, signedIn);
        assertEq("... and runs one release loop (65, then 85 and 125)", 2, seat.releases.size());
    }

    private static void reconnectDuringAnOutageRunsOneRetryLoop(Stub stub) {
        Rig rig = new Rig(stub);
        UUID id = stub.player(false, false);
        stub.failWith = 500;
        rig.join(id, "Flaky"); // first try fails, the next is due at 20
        rig.gate.leave(id);
        rig.gate.advance(5);
        Seat again = rig.join(id, "Flaky"); // fails at 5, the next is due at 25
        rig.gate.advance(20);
        assertEq("the old retry was replaced: tries at 5 and 25 only", 2, again.releases.size());
        stub.failWith = 0;
        rig.gate.advance(40);
        assertEq("one code once felis-api is back", 1, again.books.size());
    }

    // ---- around the flow ----

    private static void readiness() throws Exception {
        Readiness r = Readiness.start(0);
        try {
            HttpClient http = HttpClient.newHttpClient();
            URI uri = URI.create("http://127.0.0.1:" + r.port() + Readiness.PATH);
            HttpResponse<String> res = http.send(HttpRequest.newBuilder(uri).build(), HttpResponse.BodyHandlers.ofString());
            assertEq("readiness before the first tick", "503 starting", res.statusCode() + " " + res.body());
            assertTrue("the first markReady flips it", r.markReady());
            assertTrue("a second one does not", !r.markReady());
            res = http.send(HttpRequest.newBuilder(uri).build(), HttpResponse.BodyHandlers.ofString());
            assertEq("readiness after the first tick", "200 ok", res.statusCode() + " " + res.body());
            try {
                Readiness.start(r.port()).stop();
                throw new AssertionError("a second endpoint bound a taken port");
            } catch (IOException expected) {
                checks++; // the plugin then stays NotReady
            }
        } finally {
            r.stop();
        }
    }

    private static void gateConfig() {
        Map<String, Integer> ports = new HashMap<>();
        ports.put(null, 8080);
        ports.put("", 8080);
        ports.put("abc", 8080);
        ports.put("0", 8080);
        ports.put("65536", 8080);
        ports.put(" 9090 ", 9090);
        ports.forEach((raw, want) -> assertEq("health port " + raw, want, GateConfig.healthPort(raw)));

        Map<String, Long> windows = new HashMap<>();
        windows.put(null, 600L);
        windows.put("soon", 600L);
        windows.put("5", 30L);
        windows.put("99999", 3600L);
        windows.put(" 120 ", 120L);
        windows.forEach((raw, want) -> assertEq("login window " + raw, want, GateConfig.loginTimeoutSeconds(raw)));

        assertEq("panel host wins", "panel.example.test", GateConfig.panelHost(" panel.example.test ", "example.test"));
        assertEq("console.<root> without it", "console.example.test", GateConfig.panelHost(" ", "example.test"));
        assertEq("neither: login stays off", null, GateConfig.panelHost(null, ""));
        assertEq("lobby default", "lobby", GateConfig.lobby(null));
        assertEq("lobby set", "hub", GateConfig.lobby(" hub "));
    }

    private static void loginBook() {
        Book book = LoginBook.book(CODE, CONSOLE + "/link");
        assertEq("book title", "Felis Login", plain(book.title()));
        Component page = book.pages().get(0);
        assertTrue("the page shows the code", plain(page).contains(CODE));
        List<ClickEvent> clicks = new ArrayList<>();
        collectClicks(page, clicks);
        assertEq("one click, opening the login page", List.of(ClickEvent.openUrl(CONSOLE + "/link")), clicks);
        assertTrue("the page says to use the system browser", plain(page).contains("SYSTEM browser"));

        // A chat line keeps its text; only the address in it turns into a link.
        Component line = LoginBook.chatLine("§e[Felis] 打开 §b" + CONSOLE + "/link §e完成登录（勿用微信/QQ内置浏览器）。");
        assertEq("chat line text", "[Felis] 打开 " + CONSOLE + "/link 完成登录（勿用微信/QQ内置浏览器）。", plain(line));
        List<Component> links = new ArrayList<>();
        collectLinks(line, links);
        assertEq("one link in the line", 1, links.size());
        assertEq("the link is the address alone", CONSOLE + "/link", plain(links.get(0)));
        assertEq("the link opens it", ClickEvent.openUrl(CONSOLE + "/link"), links.get(0).clickEvent());
        assertEq("the link looks like one", TextDecoration.State.TRUE, links.get(0).decoration(TextDecoration.UNDERLINED));
        List<ClickEvent> none = new ArrayList<>();
        collectClicks(LoginBook.chatLine(LoginFlow.SIGNED_IN), none);
        assertEq("a line without an address has no link", List.of(), none);
    }

    // ---- fakes ----

    /** Rig is one fresh gate and flow over the shared stub. */
    private static final class Rig {
        final Stub stub;
        final FakeGate gate = new FakeGate();
        final LoginFlow flow;

        Rig(Stub stub) {
            this(stub, 600_000L);
        }

        Rig(Stub stub, long timeoutMillis) {
            this.stub = stub;
            LinkConfig cfg = new LinkConfig(stub.base(), "gate-token");
            flow = new LoginFlow(gate, new FelisApiClient(cfg), new LinkClient(cfg), CONSOLE, timeoutMillis,
                    gate::now, QUIET);
        }

        /** join seats a player, fires the join and runs what is due right away. */
        Seat join(UUID id, String name) {
            Seat seat = gate.seat(id, name);
            flow.join(id);
            gate.advance(0);
            return seat;
        }
    }

    /**
     * FakeGate is a Limbo scheduler on a virtual clock (a tick is 50 ms): tasks run on
     * the test thread in due order when the test advances time, sync and async alike.
     */
    private static final class FakeGate implements LoginFlow.Gate {
        private static final class Task {
            final int id;
            final Runnable body;
            final long period;
            long due;
            long seq;

            Task(int id, Runnable body, long due, long period) {
                this.id = id;
                this.body = body;
                this.due = due;
                this.period = period;
            }
        }

        private final Map<UUID, Seat> online = new HashMap<>();
        private final Map<Integer, Task> tasks = new HashMap<>();
        private int nextId = 1;
        private long nextSeq;
        private long tick;
        // syncLag delays every sync task, as a main thread running behind would.
        long syncLag;

        long now() {
            return 1_700_000_000_000L + tick * 50L;
        }

        Seat seat(UUID id, String name) {
            Seat seat = new Seat(name);
            online.put(id, seat);
            return seat;
        }

        void leave(UUID id) {
            online.remove(id);
        }

        int pending() {
            return tasks.size();
        }

        void advance(long ticks) {
            long target = tick + ticks;
            while (true) {
                Task next = null;
                for (Task t : tasks.values()) {
                    if (t.due <= target && (next == null || t.due < next.due || (t.due == next.due && t.seq < next.seq))) {
                        next = t;
                    }
                }
                if (next == null) {
                    break;
                }
                tick = Math.max(tick, next.due);
                if (next.period > 0) {
                    next.due = tick + next.period;
                    next.seq = nextSeq++;
                } else {
                    tasks.remove(next.id);
                }
                next.body.run();
            }
            tick = target;
        }

        private int schedule(Runnable body, long delay, long period) {
            Task t = new Task(nextId++, body, tick + delay, period);
            t.seq = nextSeq++;
            tasks.put(t.id, t);
            return t.id;
        }

        @Override
        public LoginFlow.Seat player(UUID id) {
            return online.get(id);
        }

        @Override
        public int runSync(Runnable task) {
            return schedule(task, syncLag, 0);
        }

        @Override
        public int runAsync(Runnable task) {
            return schedule(task, 0, 0);
        }

        @Override
        public int runLater(Runnable task, long delayTicks) {
            return schedule(task, delayTicks, 0);
        }

        @Override
        public int runLaterAsync(Runnable task, long delayTicks) {
            return schedule(task, delayTicks, 0);
        }

        @Override
        public int runTimerAsync(Runnable task, long delayTicks, long periodTicks) {
            return schedule(task, delayTicks, periodTicks);
        }

        @Override
        public void cancel(int taskId) {
            tasks.remove(taskId);
        }
    }

    /** Seat records what the gate did to one player. */
    private static final class Seat implements LoginFlow.Seat {
        final String name;
        final List<String> chat = new ArrayList<>();
        final List<String> books = new ArrayList<>();
        final List<ControlFrame> releases = new ArrayList<>();
        String disconnected;
        boolean refusesBook;

        Seat(String name) {
            this.name = name;
        }

        @Override
        public String name() {
            return name;
        }

        @Override
        public void chat(String line) {
            chat.add(line);
        }

        @Override
        public void showCode(LinkCode code, String url) {
            if (refusesBook) {
                throw new IllegalStateException("client refused the book");
            }
            books.add(code.code() + " " + url);
        }

        @Override
        public void sendControl(byte[] frame) {
            releases.add(Control.decode(frame));
        }

        @Override
        public void disconnect(String reason) {
            if (disconnected != null) {
                throw new AssertionError("disconnected twice: " + disconnected + " then " + reason);
            }
            disconnected = reason;
        }
    }

    /**
     * Stub is felis-api's internal face as the gate uses it: the blacklist, the link
     * status and the code mint, with a switch that fails every call with one status.
     */
    private static final class Stub {
        final HttpServer http;
        final Set<UUID> barred = ConcurrentHashMap.newKeySet();
        final Map<UUID, Boolean> linked = new ConcurrentHashMap<>();
        final Map<String, AtomicInteger> hits = new ConcurrentHashMap<>();
        final AtomicInteger mints = new AtomicInteger();
        volatile int failWith;
        volatile int mintStatus = 201;
        volatile String panelUrl;

        Stub() throws IOException {
            http = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
            http.createContext("/", this::handle);
            http.start();
        }

        String base() {
            return "http://127.0.0.1:" + http.getAddress().getPort();
        }

        UUID player(boolean isBarred, boolean isLinked) {
            UUID id = UUID.randomUUID();
            if (isBarred) {
                barred.add(id);
            }
            linked.put(id, isLinked);
            return id;
        }

        int hits(String part) {
            AtomicInteger n = hits.get(part);
            return n == null ? 0 : n.get();
        }

        void stop() {
            http.stop(0);
        }

        private void handle(HttpExchange ex) throws IOException {
            String path = ex.getRequestURI().getPath();
            if (!"Bearer gate-token".equals(ex.getRequestHeaders().getFirst("Authorization"))) {
                reply(ex, 401, "{\"error\":{\"code\":\"unauthorized\",\"message\":\"unauthorized\"}}");
                return;
            }
            String kind = path.contains("/link/status/") ? "/link/status/"
                    : path.contains("/blacklist/") ? "/blacklist/"
                    : path.endsWith("/link/code") ? "/link/code" : path;
            hits.computeIfAbsent(kind, k -> new AtomicInteger()).incrementAndGet();
            if (failWith != 0) {
                reply(ex, failWith, "{\"error\":{\"code\":\"unavailable\",\"message\":\"unavailable\"}}");
                return;
            }
            String tail = path.substring(path.lastIndexOf('/') + 1);
            switch (kind) {
                case "/blacklist/" -> reply(ex, 200, "{\"blacklisted\":" + barred.contains(UUID.fromString(tail)) + "}");
                case "/link/status/" -> reply(ex, 200, "{\"linked\":" + linked.getOrDefault(UUID.fromString(tail), false) + "}");
                case "/link/code" -> {
                    mints.incrementAndGet();
                    if (mintStatus != 201) {
                        reply(ex, mintStatus, "{\"error\":{\"code\":\"rate_limited\",\"message\":\"slow down\"}}");
                        return;
                    }
                    String url = panelUrl;
                    reply(ex, 201, "{\"code\":\"" + CODE + "\",\"expires_at\":\"2026-09-27T12:10:00Z\""
                            + (url != null ? ",\"panel_url\":\"" + url + "\"" : "") + "}");
                }
                default -> reply(ex, 404, "{\"error\":{\"code\":\"not_found\",\"message\":\"not_found\"}}");
            }
        }

        private static void reply(HttpExchange ex, int status, String body) throws IOException {
            byte[] bytes = body.getBytes(StandardCharsets.UTF_8);
            ex.getResponseHeaders().set("Content-Type", "application/json");
            ex.sendResponseHeaders(status, bytes.length);
            try (OutputStream os = ex.getResponseBody()) {
                os.write(bytes);
            }
        }
    }

    // ---- helpers ----

    private static String plain(Component c) {
        StringBuilder sb = new StringBuilder();
        if (c instanceof TextComponent t) {
            sb.append(t.content());
        }
        for (Component child : c.children()) {
            sb.append(plain(child));
        }
        return sb.toString();
    }

    private static void collectLinks(Component c, List<Component> out) {
        if (c.clickEvent() != null) {
            out.add(c);
        }
        for (Component child : c.children()) {
            collectLinks(child, out);
        }
    }

    private static void collectClicks(Component c, List<ClickEvent> out) {
        if (c.clickEvent() != null) {
            out.add(c.clickEvent());
        }
        for (Component child : c.children()) {
            collectClicks(child, out);
        }
    }

    private static void assertTrue(String what, boolean ok) {
        if (!ok) {
            throw new AssertionError(what);
        }
        checks++;
    }

    private static void assertEq(String what, Object want, Object got) {
        if (want == null ? got != null : !want.equals(got)) {
            throw new AssertionError(what + ": got " + got + ", want " + want);
        }
        checks++;
    }
}
