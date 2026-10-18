package best.lolicon.felis.velocity;

import java.util.concurrent.atomic.AtomicBoolean;

// Skip scheduled runs while the previous invocation is still active.
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
