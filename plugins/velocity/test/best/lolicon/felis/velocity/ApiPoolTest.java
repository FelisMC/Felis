package best.lolicon.felis.velocity;

import java.util.List;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;

/**
 * ApiPoolTest covers the two guards around the proxy's felis-api calls:
 * {@link BoundedExecutor} (a fixed number of calls in flight, a bounded queue, refusal
 * past it, a throwing task logged and survived) and {@link SkipIfRunning} (a repeating
 * task never overlaps itself, and a run that throws does not block later runs).
 * Framework free: a failed assertion throws and the process exits non-zero.
 *
 * <p>Run: {@code javac -d <out> velocity/src/main/java/best/lolicon/felis/velocity/{BoundedExecutor,SkipIfRunning}.java
 * velocity/test/best/lolicon/felis/velocity/ApiPoolTest.java && java -cp <out>
 * best.lolicon.felis.velocity.ApiPoolTest}.
 */
public final class ApiPoolTest {

    private static int checks;

    public static void main(String[] args) throws Exception {
        fullPoolRefusesInsteadOfGrowing();
        throwingTaskIsReportedAndThePoolLives();
        shutdownRefuses();
        repeatingTaskNeverOverlaps();
        throwingRunDoesNotWedgeTheGuard();
        System.out.println("ApiPoolTest OK (" + checks + " checks)");
    }

    // Two threads, two queue slots: four tasks are taken (two running, two waiting), the
    // fifth is refused; once the blockers finish, every accepted task runs.
    private static void fullPoolRefusesInsteadOfGrowing() throws Exception {
        BoundedExecutor pool = new BoundedExecutor("t", 2, 2, t -> { });
        CountDownLatch started = new CountDownLatch(2);
        CountDownLatch release = new CountDownLatch(1);
        AtomicInteger ran = new AtomicInteger();
        Runnable blocker = () -> {
            started.countDown();
            await(release);
            ran.incrementAndGet();
        };
        assertEq("first taken", true, pool.submit(blocker));
        assertEq("second taken", true, pool.submit(blocker));
        assertEq("both blockers running", true, started.await(5, TimeUnit.SECONDS));
        assertEq("running while blocked", 2, pool.running());
        assertEq("third queued", true, pool.submit(ran::incrementAndGet));
        assertEq("fourth queued", true, pool.submit(ran::incrementAndGet));
        assertEq("waiting while blocked", 2, pool.waiting());
        assertEq("fifth refused", false, pool.submit(ran::incrementAndGet));
        assertEq("nothing finished yet", 0, ran.get());

        release.countDown();
        CountDownLatch drained = new CountDownLatch(1);
        waitFor(() -> ran.get() == 4, "accepted tasks all run");
        assertEq("refused task never ran", 4, ran.get());
        // Room again once drained.
        assertEq("taken after drain", true, pool.submit(drained::countDown));
        assertEq("post-drain task ran", true, drained.await(5, TimeUnit.SECONDS));
        pool.shutdown();
    }

    private static void throwingTaskIsReportedAndThePoolLives() throws Exception {
        List<Throwable> errors = new CopyOnWriteArrayList<>();
        BoundedExecutor pool = new BoundedExecutor("t", 1, 1, errors::add);
        pool.submit(() -> {
            throw new IllegalStateException("boom");
        });
        CountDownLatch next = new CountDownLatch(1);
        waitFor(() -> errors.size() == 1, "error reported");
        assertEq("reported message", "boom", errors.get(0).getMessage());
        assertEq("taken after a throw", true, pool.submit(next::countDown));
        assertEq("task after a throw ran", true, next.await(5, TimeUnit.SECONDS));
        pool.shutdown();
    }

    private static void shutdownRefuses() {
        BoundedExecutor pool = new BoundedExecutor("t", 1, 1, t -> { });
        pool.shutdown();
        assertEq("refused after shutdown", false, pool.submit(() -> { }));
    }

    // A second run that falls due while the first is inside the task is skipped, not
    // started beside it; the next run after the first finishes goes ahead.
    private static void repeatingTaskNeverOverlaps() throws Exception {
        CountDownLatch inside = new CountDownLatch(1);
        CountDownLatch release = new CountDownLatch(1);
        AtomicInteger entered = new AtomicInteger();
        AtomicInteger concurrent = new AtomicInteger();
        AtomicInteger maxConcurrent = new AtomicInteger();
        SkipIfRunning guarded = new SkipIfRunning(() -> {
            int now = concurrent.incrementAndGet();
            maxConcurrent.accumulateAndGet(now, Math::max);
            entered.incrementAndGet();
            if (entered.get() == 1) {
                inside.countDown();
                await(release);
            }
            concurrent.decrementAndGet();
        });
        Thread first = new Thread(guarded);
        first.start();
        assertEq("first run inside", true, inside.await(5, TimeUnit.SECONDS));
        guarded.run(); // due while the first is still running
        guarded.run();
        assertEq("overlapping runs skipped", 1, entered.get());
        release.countDown();
        first.join(5000);
        guarded.run();
        assertEq("run after the first finished", 2, entered.get());
        assertEq("never two at once", 1, maxConcurrent.get());
    }

    private static void throwingRunDoesNotWedgeTheGuard() {
        AtomicInteger calls = new AtomicInteger();
        SkipIfRunning guarded = new SkipIfRunning(() -> {
            if (calls.incrementAndGet() == 1) {
                throw new IllegalStateException("felis-api down");
            }
        });
        try {
            guarded.run();
            throw new AssertionError("the first run's exception was swallowed");
        } catch (IllegalStateException e) {
            assertEq("first run's exception propagates", "felis-api down", e.getMessage());
        }
        guarded.run();
        assertEq("runs after a throw", 2, calls.get());
    }

    // ---- harness ----

    interface Cond {
        boolean ok();
    }

    private static void waitFor(Cond cond, String what) throws InterruptedException {
        long deadline = System.nanoTime() + TimeUnit.SECONDS.toNanos(5);
        while (!cond.ok()) {
            if (System.nanoTime() > deadline) {
                throw new AssertionError(what + ": timed out");
            }
            Thread.sleep(5);
        }
        checks++;
    }

    private static void await(CountDownLatch l) {
        try {
            l.await(5, TimeUnit.SECONDS);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
        }
    }

    private static void assertEq(String what, Object want, Object got) {
        if (!want.equals(got)) {
            throw new AssertionError(what + ": got " + got + ", want " + want);
        }
        checks++;
    }
}
