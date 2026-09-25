package best.lolicon.felis.velocity;

import java.util.concurrent.atomic.AtomicBoolean;

/**
 * SkipIfRunning wraps a repeating task so that a run falling due while the previous
 * one is still going is skipped rather than started beside it. Velocity's
 * {@code repeat()} fires on schedule whatever the last run is doing, so a felis-api
 * call slower than the interval (a 10 s request timeout against the 15 s registration
 * refresh, or several calls in one pass) stacked runs that each held a thread and
 * sent the same requests again, adding load exactly when the API was struggling.
 */
final class SkipIfRunning implements Runnable {
    private final Runnable task;
    private final AtomicBoolean running = new AtomicBoolean();

    SkipIfRunning(Runnable task) {
        this.task = task;
    }

    @Override
    public void run() {
        if (!running.compareAndSet(false, true)) {
            return;
        }
        try {
            task.run();
        } finally {
            running.set(false);
        }
    }
}
