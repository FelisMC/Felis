package best.lolicon.felis.link;

import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;

import java.io.IOException;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.util.Map;
import java.util.UUID;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.atomic.AtomicInteger;

/**
 * FelisApiClientRetryTest pins which failures FelisApiClient repeats, against a stub
 * felis-api that counts the requests each path receives: a GET is tried once more
 * after a connection that dropped mid-reply or a 502/503/504, never a third time; a POST is
 * never repeated (the first attempt may have landed); a GET that ran past the
 * request timeout is cut off at that timeout and not repeated. It then checks the
 * client's {@link ApiStats} against the requests the stub saw.
 *
 * <p>Run: {@code javac -d <out> shared/src/main/java/best/lolicon/felis/link/*.java
 * shared/test/best/lolicon/felis/link/FelisApiClientRetryTest.java && java -cp <out>
 * best.lolicon.felis.link.FelisApiClientRetryTest}.
 */
public final class FelisApiClientRetryTest {

    private static final String READY = "{\"name\":\"alpha\",\"phase\":\"Running\",\"ready\":true}";
    private static final Map<String, AtomicInteger> hits = new ConcurrentHashMap<>();
    private static int checks;

    public static void main(String[] args) throws Exception {
        HttpServer stub = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        // The slow handler sleeps; the others must not queue behind it.
        ExecutorService pool = Executors.newCachedThreadPool();
        stub.setExecutor(pool);
        stub.createContext("/", FelisApiClientRetryTest::handle);
        stub.start();
        try {
            String base = "http://127.0.0.1:" + stub.getAddress().getPort();
            FelisApiClient api = new FelisApiClient(new LinkConfig(
                    base, "test-token", Duration.ofSeconds(2), Duration.ofMillis(400)));
            getRetriesOnceAfter503(api);
            getRetriesOnceAfterACutReply(api);
            getGivesUpAfterTheSecondTry(api);
            getIsNotRetriedOnAClientError(api);
            postIsNeverRetried(api);
            timedOutGetIsCutOffAndNotRetried(api);
            statsCountEveryAttempt(api);
            statsArithmetic();
        } finally {
            stub.stop(0);
            pool.shutdownNow();
        }
        System.out.println("FelisApiClientRetryTest OK (" + checks + " checks)");
    }

    // Each server name is one scripted behaviour; the n-th request to it picks the reply.
    private static void handle(HttpExchange ex) throws IOException {
        String path = ex.getRequestURI().getRawPath();
        ex.getRequestBody().readAllBytes();
        int n = hits.computeIfAbsent(ex.getRequestMethod() + " " + path, k -> new AtomicInteger()).incrementAndGet();
        switch (path) {
            case "/api/v1/internal/servers/flaky/status":
                reply(ex, n == 1 ? 503 : 200, n == 1 ? error("unavailable") : READY);
                return;
            case "/api/v1/internal/servers/cut/status":
                if (n == 1) {
                    // Headers promise 1000 bytes, the connection drops after 10: felis-api
                    // dying mid-reply. (A drop before any response byte is not a usable
                    // probe: the JDK client resends such a GET on its own.)
                    ex.getResponseHeaders().set("Content-Type", "application/json");
                    ex.sendResponseHeaders(200, 1000);
                    ex.getResponseBody().write("{\"name\":\"a".getBytes(StandardCharsets.UTF_8));
                    ex.getResponseBody().flush();
                    ex.close();
                    return;
                }
                reply(ex, 200, READY);
                return;
            case "/api/v1/internal/servers/down/status":
                reply(ex, 502, error("bad_gateway"));
                return;
            case "/api/v1/internal/servers/gone/status":
                reply(ex, 404, error("not_found"));
                return;
            case "/api/v1/internal/servers/busy/wake":
                reply(ex, 503, error("at_capacity"));
                return;
            case "/api/v1/internal/servers/slow/status":
                try {
                    Thread.sleep(1500);
                } catch (InterruptedException e) {
                    Thread.currentThread().interrupt();
                }
                reply(ex, 200, READY);
                return;
            default:
                reply(ex, 500, error("unexpected"));
        }
    }

    private static void getRetriesOnceAfter503(FelisApiClient api) throws LinkException {
        ServerView v = api.serverStatus("flaky");
        assertEq("flaky phase", "Running", v.phase());
        assertEq("flaky requests", 2, count("GET /api/v1/internal/servers/flaky/status"));
    }

    private static void getRetriesOnceAfterACutReply(FelisApiClient api) throws LinkException {
        ServerView v = api.serverStatus("cut");
        assertEq("cut phase", "Running", v.phase());
        assertEq("cut requests", 2, count("GET /api/v1/internal/servers/cut/status"));
    }

    private static void getGivesUpAfterTheSecondTry(FelisApiClient api) {
        LinkException e = expectFailure("down", () -> api.serverStatus("down"));
        assertEq("down status", 502, e.statusCode());
        assertEq("down code", "bad_gateway", e.errorCode());
        assertEq("down requests", 2, count("GET /api/v1/internal/servers/down/status"));
    }

    private static void getIsNotRetriedOnAClientError(FelisApiClient api) {
        LinkException e = expectFailure("gone", () -> api.serverStatus("gone"));
        assertEq("gone status", 404, e.statusCode());
        assertEq("gone requests", 1, count("GET /api/v1/internal/servers/gone/status"));
    }

    private static void postIsNeverRetried(FelisApiClient api) {
        UUID id = UUID.fromString("00000000-0000-0000-0000-00000000000a");
        LinkException e = expectFailure("busy", () -> api.wake("busy", id));
        assertEq("busy status", 503, e.statusCode());
        assertEq("busy code", "at_capacity", e.errorCode());
        assertEq("busy requests", 1, count("POST /api/v1/internal/servers/busy/wake"));
    }

    private static void timedOutGetIsCutOffAndNotRetried(FelisApiClient api) {
        long start = System.nanoTime();
        LinkException e = expectFailure("slow", () -> api.serverStatus("slow"));
        long tookMillis = (System.nanoTime() - start) / 1_000_000;
        assertEq("slow status", 0, e.statusCode());
        assertEq("slow code", "transport_error", e.errorCode());
        // 400 ms request timeout against a 1500 ms handler: well under the handler's
        // time, which a missing or ignored request timeout would have to wait out.
        if (tookMillis >= 1200) {
            throw new AssertionError("slow: took " + tookMillis + " ms, want the 400 ms request timeout");
        }
        checks++;
        assertEq("slow requests", 1, count("GET /api/v1/internal/servers/slow/status"));
    }

    // The calls above, attempt by attempt: flaky 503+200, cut dropped+200, down 502+502,
    // gone 404, busy 503 (POST), slow timed out. Nine attempts, three of them retries;
    // no answer twice (the cut reply, the timeout), one 4xx, four 5xx.
    private static void statsCountEveryAttempt(FelisApiClient api) throws LinkException {
        ApiStats.Snapshot t = api.stats().total();
        int sent = hits.values().stream().mapToInt(AtomicInteger::get).sum();
        assertEq("stats calls = requests the stub saw", (long) sent, t.calls);
        assertEq("stats calls", 9L, t.calls);
        assertEq("stats no answer", 2L, t.transport);
        assertEq("stats 4xx", 1L, t.clientErrors);
        assertEq("stats 5xx", 4L, t.serverErrors);
        assertEq("stats retried", 3L, t.retries);
        // The slowest attempt is the one cut off by the 400 ms request timeout.
        if (t.maxMillis < 390 || t.maxMillis >= 1200) {
            throw new AssertionError("stats max: " + t.maxMillis + " ms, want the ~400 ms timeout");
        }
        checks++;

        ApiStats.Snapshot w1 = api.stats().window();
        assertEq("first window = totals so far", 9L, w1.calls);
        assertEq("first window 5xx", 4L, w1.serverErrors);
        assertEq("first window max", t.maxMillis, w1.maxMillis);
        ApiStats.Snapshot w2 = api.stats().window();
        assertEq("empty window calls", 0L, w2.calls);
        assertEq("empty window max", 0L, w2.maxMillis);
        assertEq("empty window avg", 0L, w2.avgMillis());
        assertEq("total max outlives the windows", t.maxMillis, api.stats().total().maxMillis);
        api.serverStatus("flaky"); // answers 200 from now on
        ApiStats.Snapshot w3 = api.stats().window();
        assertEq("next window calls", 1L, w3.calls);
        assertEq("next window 5xx", 0L, w3.serverErrors);
        assertEq("next window retried", 0L, w3.retries);
        assertEq("totals keep growing", 10L, api.stats().total().calls);
    }

    // The status classes at their edges and the latency figures, on fixed inputs.
    private static void statsArithmetic() {
        ApiStats s = new ApiStats();
        s.record(200, 10);
        s.record(399, 40);
        s.record(400, 30);
        s.record(499, 5);
        s.record(500, 20);
        s.record(0, 15);
        ApiStats.Snapshot t = s.total();
        assertEq("edge calls", 6L, t.calls);
        assertEq("edge 4xx (400, 499)", 2L, t.clientErrors);
        assertEq("edge 5xx (500)", 1L, t.serverErrors);
        assertEq("edge no answer", 1L, t.transport);
        assertEq("avg of 10+40+30+5+20+15 over 6", 20L, t.avgMillis());
        assertEq("max is the slowest, not the last", 40L, t.maxMillis);
        assertEq("summary", "calls=6 (no answer=1, 4xx=2, 5xx=1, retried=0), avg=20 ms, max=40 ms",
                t.summary());
    }

    // ---- harness ----

    interface Call {
        void run() throws LinkException;
    }

    private static LinkException expectFailure(String what, Call call) {
        try {
            call.run();
        } catch (LinkException e) {
            return e;
        }
        throw new AssertionError(what + ": call succeeded, want a LinkException");
    }

    private static int count(String key) {
        AtomicInteger n = hits.get(key);
        return n == null ? 0 : n.get();
    }

    private static String error(String code) {
        return "{\"error\":{\"code\":\"" + code + "\",\"message\":\"stub\"}}";
    }

    private static void reply(HttpExchange ex, int status, String body) throws IOException {
        byte[] b = body.getBytes(StandardCharsets.UTF_8);
        ex.getResponseHeaders().set("Content-Type", "application/json");
        ex.sendResponseHeaders(status, b.length);
        try (OutputStream os = ex.getResponseBody()) {
            os.write(b);
        }
    }

    private static void assertEq(String what, Object want, Object got) {
        if (!want.equals(got)) {
            throw new AssertionError(what + ": got " + got + ", want " + want);
        }
        checks++;
    }
}
