package best.lolicon.felis.velocity;

import best.lolicon.felis.link.ApiStats;
import best.lolicon.felis.link.FelisApiClient;
import best.lolicon.felis.link.LinkConfig;
import best.lolicon.felis.link.LinkException;

import java.io.IOException;
import java.net.InetAddress;
import java.net.ServerSocket;
import java.util.UUID;

/**
 * ProxyStatsTest checks the proxy's failure counters: windows never overlap, totals
 * keep growing, and the periodic line names every counter with its window value and
 * stays silent for an idle window. Framework free: a failed assertion throws.
 *
 * <p>Run: {@code javac -d <out> shared/src/main/java/best/lolicon/felis/link/*.java
 * velocity/src/main/java/best/lolicon/felis/velocity/ProxyStats.java
 * velocity/test/best/lolicon/felis/velocity/ProxyStatsTest.java && java -cp <out>
 * best.lolicon.felis.velocity.ProxyStatsTest}.
 */
public final class ProxyStatsTest {

    private static int checks;

    public static void main(String[] args) throws IOException {
        ProxyStats s = new ProxyStats();
        s.count(ProxyStats.Event.JOIN_EVENT_FAILED);
        s.count(ProxyStats.Event.JOIN_EVENT_FAILED);
        s.count(ProxyStats.Event.TRANSFER_FAILED);
        s.count(ProxyStats.Event.REFRESH_FAILED);
        s.count(ProxyStats.Event.REFRESH_FAILED);
        s.count(ProxyStats.Event.REFRESH_FAILED);

        long[] w1 = s.window();
        assertEq("w1 pool refused", 0L, w1[ProxyStats.Event.POOL_REFUSED.ordinal()]);
        assertEq("w1 join failed", 2L, w1[ProxyStats.Event.JOIN_EVENT_FAILED.ordinal()]);
        assertEq("w1 join dropped", 0L, w1[ProxyStats.Event.JOIN_EVENT_DROPPED.ordinal()]);
        assertEq("w1 transfer failed", 1L, w1[ProxyStats.Event.TRANSFER_FAILED.ordinal()]);
        assertEq("w1 refresh failed", 3L, w1[ProxyStats.Event.REFRESH_FAILED.ordinal()]);

        s.count(ProxyStats.Event.POOL_REFUSED);
        s.count(ProxyStats.Event.JOIN_EVENT_FAILED);
        long[] w2 = s.window();
        assertEq("w2 pool refused", 1L, w2[ProxyStats.Event.POOL_REFUSED.ordinal()]);
        assertEq("w2 join failed (only the new one)", 1L, w2[ProxyStats.Event.JOIN_EVENT_FAILED.ordinal()]);
        assertEq("w2 refresh failed (none new)", 0L, w2[ProxyStats.Event.REFRESH_FAILED.ordinal()]);
        assertEq("total join failed", 3L, s.total(ProxyStats.Event.JOIN_EVENT_FAILED));
        assertEq("total refresh failed", 3L, s.total(ProxyStats.Event.REFRESH_FAILED));

        long[] idle = s.window();
        ApiStats noCalls = new ApiStats();
        assertEq("idle window logs nothing", null, ProxyStats.line(10, noCalls.window(), idle, 0));
        assertEq("someone waiting is worth a line",
                "Felis: last 10 min: felis-api calls=0 (no answer=0, 4xx=0, 5xx=0, retried=0), avg=0 ms, max=0 ms"
                        + ", busy refusals=0, join-events failed=0, join-events dropped=0, transfers failed=0"
                        + ", server-list refreshes failed=0, waiting now=2",
                ProxyStats.line(10, noCalls.window(), idle, 2));
        assertEq("a failure alone is worth a line",
                "Felis: last 10 min: felis-api calls=0 (no answer=0, 4xx=0, 5xx=0, retried=0), avg=0 ms, max=0 ms"
                        + ", busy refusals=1, join-events failed=1, join-events dropped=0, transfers failed=0"
                        + ", server-list refreshes failed=0, waiting now=0",
                ProxyStats.line(10, noCalls.window(), w2, 0));

        // One felis-api call and nothing else is still worth a line: a single POST to a
        // port nobody listens on (POST is never retried, so exactly one attempt).
        int closed;
        try (ServerSocket probe = new ServerSocket(0, 1, InetAddress.getLoopbackAddress())) {
            closed = probe.getLocalPort();
        }
        FelisApiClient api = new FelisApiClient(new LinkConfig("http://127.0.0.1:" + closed, "t"));
        try {
            api.reportJoin("lobby", UUID.fromString("00000000-0000-0000-0000-000000000001"));
            throw new AssertionError("reportJoin to a closed port succeeded");
        } catch (LinkException expected) {
            // the call failed without an answer, which is what we want counted
        }
        String one = ProxyStats.line(10, api.stats().window(), s.window(), 0);
        assertEq("one call alone is worth a line", true, one != null && one.startsWith(
                "Felis: last 10 min: felis-api calls=1 (no answer=1, 4xx=0, 5xx=0, retried=0), avg="));

        System.out.println("ProxyStatsTest OK (" + checks + " checks)");
    }

    private static void assertEq(String what, Object want, Object got) {
        if (want == null ? got != null : !want.equals(got)) {
            throw new AssertionError(what + ": got " + got + ", want " + want);
        }
        checks++;
    }
}
