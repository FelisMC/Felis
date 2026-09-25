package best.lolicon.felis.link;

import java.util.concurrent.atomic.AtomicLong;

/**
 * OutageTrackerTest walks an {@link OutageTracker} through an outage on an injected
 * clock: one DOWN on the first failure, STILL_DOWN no more than once per repeat
 * interval, RECOVERED once with the outage's length and failure count, and silence
 * while all is well.
 *
 * <p>Run: {@code javac -d <out> shared/src/main/java/best/lolicon/felis/link/*.java
 * shared/test/best/lolicon/felis/link/OutageTrackerTest.java && java -cp <out>
 * best.lolicon.felis.link.OutageTrackerTest}.
 */
public final class OutageTrackerTest {

    private static int checks;

    public static void main(String[] args) {
        AtomicLong now = new AtomicLong(10_000L);
        OutageTracker t = new OutageTracker(60_000L, now::get);

        assertEq("success while up", OutageTracker.Report.NONE, t.success());
        assertEq("failures while up", 0L, t.failures());

        assertEq("first failure", OutageTracker.Report.DOWN, t.failure());
        now.addAndGet(2_000L);
        assertEq("second failure 2 s later", OutageTracker.Report.NONE, t.failure());
        now.addAndGet(57_999L); // 59.999 s after DOWN
        assertEq("just under the repeat interval", OutageTracker.Report.NONE, t.failure());
        now.addAndGet(1L); // 60 s after DOWN
        assertEq("repeat interval reached", OutageTracker.Report.STILL_DOWN, t.failure());
        now.addAndGet(30_000L);
        assertEq("half an interval after the reminder", OutageTracker.Report.NONE, t.failure());
        now.addAndGet(30_000L);
        assertEq("an interval after the reminder", OutageTracker.Report.STILL_DOWN, t.failure());
        assertEq("failures in the outage", 6L, t.failures());
        assertEq("outage length so far", 120_000L, t.downForMillis());

        now.addAndGet(5_000L);
        assertEq("first success", OutageTracker.Report.RECOVERED, t.success());
        assertEq("closed outage failures", 6L, t.lastOutageFailures());
        assertEq("closed outage length", 125_000L, t.lastOutageMillis());
        assertEq("failures reset", 0L, t.failures());
        assertEq("down-for reset", 0L, t.downForMillis());
        assertEq("second success", OutageTracker.Report.NONE, t.success());

        // A new outage starts its own count and is announced again at once.
        now.addAndGet(1_000L);
        assertEq("next outage", OutageTracker.Report.DOWN, t.failure());
        assertEq("next outage failures", 1L, t.failures());

        System.out.println("OutageTrackerTest OK (" + checks + " checks)");
    }

    private static void assertEq(String what, Object want, Object got) {
        if (!want.equals(got)) {
            throw new AssertionError(what + ": got " + got + ", want " + want);
        }
        checks++;
    }
}
