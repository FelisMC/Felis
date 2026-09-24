package best.lolicon.felis.velocity;

import best.lolicon.felis.link.LinkException;

import java.util.UUID;
import java.util.concurrent.atomic.AtomicLong;

/**
 * LinkGateTest covers the two rate/fallback helpers the proxy keeps in memory,
 * {@link LinkGate} and {@link FrameBudget}, on an injected clock. Framework free: a
 * failed assertion throws and the process exits non-zero.
 *
 * <p>Run: {@code javac -d <out> shared/src/main/java/best/lolicon/felis/link/*.java
 * velocity/src/main/java/best/lolicon/felis/velocity/{LinkGate,FrameBudget}.java
 * velocity/test/best/lolicon/felis/velocity/LinkGateTest.java && java -cp <out>
 * best.lolicon.felis.velocity.LinkGateTest}.
 */
public final class LinkGateTest {

    private static int checks;
    private static final AtomicLong now = new AtomicLong(1_000_000L);

    // What the stub felis-api does next: answer true/false, or fail with a status.
    private static volatile Boolean answer = Boolean.TRUE;
    private static volatile int failStatus = 0;

    private static final long GRACE = 600_000L;

    public static void main(String[] args) throws Exception {
        apiAnswerAlwaysWins();
        outageFallsBackOnlyToARecentPositive();
        clientErrorsNeverFallBack();
        pruneBoundsTheRecords();
        frameBudgetRefills();
        System.out.println("LinkGateTest OK (" + checks + " checks)");
    }

    private static LinkGate gate() {
        return new LinkGate(id -> {
            if (answer == null) {
                throw new LinkException(failStatus, "x", "stub failure");
            }
            return answer;
        }, GRACE, now::get);
    }

    private static void apiAnswerAlwaysWins() throws LinkException {
        LinkGate g = gate();
        UUID id = UUID.randomUUID();
        answer = true;
        LinkGate.Result r = g.check(id);
        assertEq("linked", true, r.linked);
        assertEq("not degraded", false, r.degraded);
        // An unlink takes effect on the very next check, grace or not.
        answer = false;
        assertEq("unlinked now", false, g.check(id).linked);
        // …and it also clears the fallback: an outage right after an unlink fails closed.
        answer = null;
        failStatus = 0;
        expectThrows("outage after unlink", g, id);
    }

    private static void outageFallsBackOnlyToARecentPositive() throws LinkException {
        LinkGate g = gate();
        UUID seen = UUID.randomUUID();
        UUID stranger = UUID.randomUUID();
        answer = true;
        g.check(seen);

        answer = null;
        failStatus = 0; // transport error
        now.addAndGet(GRACE - 1);
        LinkGate.Result r = g.check(seen);
        assertEq("fallback linked", true, r.linked);
        assertEq("fallback degraded", true, r.degraded);
        failStatus = 503;
        assertEq("5xx falls back too", true, g.check(seen).linked);
        expectThrows("never-seen UUID fails closed", g, stranger);

        now.addAndGet(2);
        expectThrows("positive older than grace fails closed", g, seen);
    }

    private static void clientErrorsNeverFallBack() throws LinkException {
        LinkGate g = gate();
        UUID id = UUID.randomUUID();
        answer = true;
        g.check(id);
        answer = null;
        for (int status : new int[] {400, 401, 403, 404, 429}) {
            failStatus = status;
            expectThrows("status " + status, g, id);
        }
    }

    private static void pruneBoundsTheRecords() throws LinkException {
        LinkGate g = gate();
        UUID id = UUID.randomUUID();
        answer = true;
        g.check(id);
        now.addAndGet(GRACE + 1);
        g.prune();
        now.addAndGet(-(GRACE + 1)); // even back inside the window, the record is gone
        answer = null;
        failStatus = 0;
        expectThrows("pruned record", g, id);
    }

    private static void frameBudgetRefills() {
        FrameBudget b = new FrameBudget(3, 2.0, now::get);
        UUID id = UUID.randomUUID();
        assertEq("t1", true, b.tryTake(id));
        assertEq("t2", true, b.tryTake(id));
        assertEq("t3", true, b.tryTake(id));
        assertEq("burst spent", false, b.tryTake(id));
        now.addAndGet(499);
        assertEq("not yet refilled", false, b.tryTake(id));
        now.addAndGet(2);
        assertEq("one token after 500ms at 2/s", true, b.tryTake(id));
        assertEq("and only one", false, b.tryTake(id));
        now.addAndGet(60_000);
        assertEq("capped at capacity 1", true, b.tryTake(id));
        assertEq("capped at capacity 2", true, b.tryTake(id));
        assertEq("capped at capacity 3", true, b.tryTake(id));
        assertEq("capped at capacity 4", false, b.tryTake(id));
        UUID other = UUID.randomUUID();
        assertEq("buckets are per player", true, b.tryTake(other));
        b.forget(id);
        assertEq("forget resets", true, b.tryTake(id));
    }

    // ---- harness ----

    private static void expectThrows(String what, LinkGate g, UUID id) {
        try {
            g.check(id);
        } catch (LinkException expected) {
            checks++;
            return;
        }
        throw new AssertionError(what + ": expected the LinkException to propagate");
    }

    private static void assertEq(String what, Object want, Object got) {
        if (want == null ? got != null : !want.equals(got)) {
            throw new AssertionError(what + " = " + got + ", want " + want);
        }
        checks++;
    }
}
