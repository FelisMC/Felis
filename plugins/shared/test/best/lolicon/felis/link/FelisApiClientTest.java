package best.lolicon.felis.link;

import com.sun.net.httpserver.HttpServer;

import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.util.List;
import java.util.UUID;
import java.util.concurrent.CopyOnWriteArrayList;

/**
 * FelisApiClientTest checks, against a stub felis-api on a loopback port, that no
 * caller-supplied value can re-aim a request at a different route. It is framework
 * free like the other test mains: a failed assertion throws and the process exits
 * non-zero.
 *
 * <p>The case that motivated it: a WakeRequest for {@code victim/join-event?} used to
 * become {@code POST /api/v1/internal/servers/victim/join-event}, which appends the
 * sender to another tenant's allowlist. The stub records every raw request path, so
 * "refused" here means no request reached the server at all.
 *
 * <p>Run: {@code javac -d <out> shared/src/main/java/best/lolicon/felis/link/*.java
 * shared/test/best/lolicon/felis/link/FelisApiClientTest.java && java -cp <out>
 * best.lolicon.felis.link.FelisApiClientTest}.
 */
public final class FelisApiClientTest {

    private static int checks;
    private static final List<String> seen = new CopyOnWriteArrayList<>();

    public static void main(String[] args) throws Exception {
        HttpServer stub = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        stub.createContext("/", exchange -> {
            seen.add(exchange.getRequestMethod() + " " + exchange.getRequestURI().getRawPath());
            String path = exchange.getRequestURI().getRawPath();
            String body;
            int status;
            if (path.endsWith("/wake")) {
                status = 202;
                body = "{\"name\":\"alpha\",\"phase\":\"Starting\",\"ready\":false}";
            } else if (path.endsWith("/approve")) {
                status = 200;
                body = "{\"approved\":true}";
            } else {
                status = 200;
                body = "{\"name\":\"alpha\",\"phase\":\"Running\",\"ready\":true,\"claimable\":false}";
            }
            byte[] b = body.getBytes(StandardCharsets.UTF_8);
            exchange.getResponseHeaders().set("Content-Type", "application/json");
            exchange.sendResponseHeaders(status, b.length);
            try (OutputStream os = exchange.getResponseBody()) {
                os.write(b);
            }
        });
        stub.start();
        try {
            FelisApiClient api = new FelisApiClient(new LinkConfig(
                    "http://127.0.0.1:" + stub.getAddress().getPort(), "test-token"));
            wellFormedNamesReachTheirRoute(api);
            pathBendingNamesNeverLeaveTheClient(api);
            opaqueSegmentsArePercentEncoded(api);
        } finally {
            stub.stop(0);
        }
        System.out.println("FelisApiClientTest OK (" + checks + " checks)");
    }

    private static void wellFormedNamesReachTheirRoute(FelisApiClient api) throws LinkException {
        seen.clear();
        UUID id = UUID.fromString("00000000-0000-0000-0000-000000000001");
        api.wake("alpha", id);
        api.menuStatus("my-server-2");
        api.serverStatus("abc");
        assertEq("routes hit", List.of(
                "POST /api/v1/internal/servers/alpha/wake",
                "GET /api/v1/internal/servers/my-server-2/menu",
                "GET /api/v1/internal/servers/abc/status"), seen);
    }

    private static void pathBendingNamesNeverLeaveTheClient(FelisApiClient api) {
        UUID id = UUID.fromString("00000000-0000-0000-0000-000000000002");
        String[] bad = {
            "victim/join-event?",   // the reported re-aim
            "victim/join-event",
            "../account/link/code",
            "a b",                  // used to throw IllegalArgumentException out of URI.create
            "Alpha",                // server names are lower-case
            "-lead",
            "trail-",
            "ab",                   // too short
            "a".repeat(33),         // too long
            "alpha#frag",
            "alpha%2Fjoin-event",
            "",
        };
        seen.clear();
        for (String name : bad) {
            expectRefused("wake " + name, () -> api.wake(name, id));
            expectRefused("reportJoin " + name, () -> api.reportJoin(name, id));
            expectRefused("claim " + name, () -> api.claim(name, id));
            expectRefused("menuStatus " + name, () -> api.menuStatus(name));
            expectRefused("serverStatus " + name, () -> api.serverStatus(name));
        }
        assertEq("requests sent for malformed names", List.of(), seen);
    }

    private static void opaqueSegmentsArePercentEncoded(FelisApiClient api) throws LinkException {
        assertEq("slash", "a%2Fb", FelisApiClient.segment("a/b"));
        assertEq("query", "a%3Fb", FelisApiClient.segment("a?b"));
        assertEq("space", "a%20b", FelisApiClient.segment("a b"));
        expectRefused("dot", () -> FelisApiClient.segment("."));
        expectRefused("dotdot", () -> FelisApiClient.segment(".."));

        seen.clear();
        api.opLoginApprove("0123abcd", UUID.fromString("00000000-0000-0000-0000-000000000003"));
        assertEq("approve route", List.of("POST /api/v1/internal/op-login/0123abcd/approve"), seen);
    }

    // ---- harness ----

    interface Call {
        void run() throws LinkException;
    }

    private static void expectRefused(String what, Call call) {
        try {
            call.run();
        } catch (LinkException e) {
            if (e.statusCode() != 400) {
                throw new AssertionError(what + ": refused with status " + e.statusCode() + ", want 400");
            }
            checks++;
            return;
        } catch (RuntimeException e) {
            throw new AssertionError(what + ": threw " + e + " instead of a LinkException", e);
        }
        throw new AssertionError(what + ": was not refused");
    }

    private static void assertEq(String what, Object want, Object got) {
        if (want == null ? got != null : !want.equals(got)) {
            throw new AssertionError(what + " = " + got + ", want " + want);
        }
        checks++;
    }
}
