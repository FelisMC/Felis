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
 * request timeout is cut off at that timeout and not repeated.
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
