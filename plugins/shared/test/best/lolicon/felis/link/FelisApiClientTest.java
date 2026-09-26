package best.lolicon.felis.link;

import com.sun.net.httpserver.HttpServer;

import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.time.Instant;
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
    private static final List<String> bodies = new CopyOnWriteArrayList<>();

    public static void main(String[] args) throws Exception {
        HttpServer stub = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        stub.createContext("/", exchange -> {
            String query = exchange.getRequestURI().getRawQuery();
            seen.add(exchange.getRequestMethod() + " " + exchange.getRequestURI().getRawPath()
                    + (query == null ? "" : "?" + query));
            bodies.add(new String(exchange.getRequestBody().readAllBytes(), StandardCharsets.UTF_8));
            String path = exchange.getRequestURI().getRawPath();
            String body;
            int status;
            if (path.endsWith("/wake")) {
                status = 202;
                body = "{\"name\":\"alpha\",\"phase\":\"Starting\",\"ready\":false}";
            } else if (path.startsWith("/api/v1/internal/player/menu-access/")) {
                status = 200;
                body = "{\"servers\":{\"mine\":\"owner\",\"odd\":7,\"pub\":\"wake\"}}";
            } else if (path.equals("/api/v1/internal/op-login/mismatch/approve")) {
                status = 409;
                body = "{\"error\":{\"code\":\"op_login_mismatch\",\"message\":\"that operator login is for a different account\"}}";
            } else if (path.equals("/api/v1/internal/op-login/half/approve")) {
                status = 200;
                body = "{\"username\":\"op\"}";
            } else if (path.endsWith("/approve")) {
                status = 200;
                body = "{\"approved\":true,\"username\":\"op\",\"email\":\"Op@Example.NET\"}";
            } else if (path.startsWith("/api/v1/internal/op-login/")) {
                status = 200;
                body = "{\"request_id\":\"0123abcd\",\"username\":\"op\",\"email\":\"Op@Example.NET\","
                        + "\"client_ip\":\"203.0.113.9\",\"user_agent\":\"Firefox/140.0\","
                        + "\"created_at\":\"2023-11-14T22:13:20Z\",\"expires_at\":\"2023-11-14T22:23:20Z\"}";
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
            opLoginShowNamesTheAccount(api);
            opLoginApproveSendsTheTypedName(api);
            menuAccessReadsVerdictsByName(api);
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

    // menuAccess reads the verdict map for the player's own UUID and keeps only the
    // string verdicts, so one malformed entry cannot hide the others.
    private static void menuAccessReadsVerdictsByName(FelisApiClient api) throws LinkException {
        seen.clear();
        UUID id = UUID.fromString("00000000-0000-0000-0000-00000000000a");
        assertEq("verdicts", java.util.Map.of("mine", "owner", "pub", "wake"), api.menuAccess(id));
        assertEq("route", List.of("GET /api/v1/internal/player/menu-access/" + id), seen);
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
        api.opLoginApprove("0123abcd", UUID.fromString("00000000-0000-0000-0000-000000000003"), "op");
        api.opLoginShow("a/b", UUID.fromString("00000000-0000-0000-0000-000000000003"));
        assertEq("op-login routes", List.of(
                "POST /api/v1/internal/op-login/0123abcd/approve",
                "GET /api/v1/internal/op-login/a%2Fb?approver_uuid=00000000-0000-0000-0000-000000000003"), seen);
    }

    // The in-game card is built from this view, so every field the admin reads has to
    // come through.
    private static void opLoginShowNamesTheAccount(FelisApiClient api) throws LinkException {
        seen.clear();
        OpLoginView v = api.opLoginShow("0123abcd", UUID.fromString("00000000-0000-0000-0000-000000000004"));
        assertEq("show route", List.of(
                "GET /api/v1/internal/op-login/0123abcd?approver_uuid=00000000-0000-0000-0000-000000000004"), seen);
        assertEq("show request_id", "0123abcd", v.requestId());
        assertEq("show username", "op", v.username());
        assertEq("show email", "Op@Example.NET", v.email());
        assertEq("show client_ip", "203.0.113.9", v.clientIp());
        assertEq("show user_agent", "Firefox/140.0", v.userAgent());
        assertEq("show created_at", Instant.parse("2023-11-14T22:13:20Z"), v.createdAt());
    }

    // The typed name is player input, so it has to arrive as one JSON string however
    // it is spelled; the approved account comes back for the confirmation line.
    private static void opLoginApproveSendsTheTypedName(FelisApiClient api) throws LinkException {
        UUID approver = UUID.fromString("00000000-0000-0000-0000-000000000005");
        bodies.clear();
        OpLoginView v = api.opLoginApprove("0123abcd", approver, "o\"p\\");
        assertEq("approve body", List.of(
                "{\"approver_uuid\":\"00000000-0000-0000-0000-000000000005\",\"username\":\"o\\\"p\\\\\"}"), bodies);
        assertEq("approved username", "op", v.username());
        assertEq("approved email", "Op@Example.NET", v.email());

        try {
            api.opLoginApprove("mismatch", approver, "alice");
            throw new AssertionError("a 409 approve was not refused");
        } catch (LinkException e) {
            assertEq("mismatch status", 409, e.statusCode());
            assertEq("mismatch code", "op_login_mismatch", e.errorCode());
        }
        try {
            api.opLoginApprove("half", approver, "op");
            throw new AssertionError("a 200 without approved=true was accepted");
        } catch (LinkException e) {
            assertEq("half status", 200, e.statusCode());
            assertEq("half code", "bad_response", e.errorCode());
        }
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
