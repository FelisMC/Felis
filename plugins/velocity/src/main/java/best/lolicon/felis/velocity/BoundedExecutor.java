package best.lolicon.felis.velocity;

import java.util.concurrent.ArrayBlockingQueue;
import java.util.concurrent.RejectedExecutionException;
import java.util.concurrent.ThreadPoolExecutor;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.function.Consumer;

// Bound blocking API work by thread and queue limits.
// Reject overflow immediately; log task exceptions without losing workers.
final class BoundedExecutor {
    private final ThreadPoolExecutor pool;
    private final Consumer<Throwable> onError;

    BoundedExecutor(String name, int threads, int queue, Consumer<Throwable> onError) {
        this.onError = onError;
        AtomicInteger n = new AtomicInteger();
        this.pool = new ThreadPoolExecutor(threads, threads, 0L, TimeUnit.MILLISECONDS,
                new ArrayBlockingQueue<>(queue),
                r -> {
                    Thread t = new Thread(r, name + "-" + n.incrementAndGet());
                    t.setDaemon(true);
                    return t;
                },
                new ThreadPoolExecutor.AbortPolicy());
    }

    /** submit queues the task, or returns false when every thread is busy and the queue is full, or after shutdown. */
    boolean submit(Runnable task) {
        try {
            pool.execute(() -> {
                try {
                    task.run();
                } catch (Throwable t) {
                    onError.accept(t);
                }
            });
            return true;
        } catch (RejectedExecutionException e) {
            return false;
        }
    }

    /** running is how many tasks are executing now; waiting how many are queued. For the busy log line. */
    int running() {
        return pool.getActiveCount();
    }

    int waiting() {
        return pool.getQueue().size();
    }

    /** shutdown stops taking tasks and interrupts the running ones (the proxy is going down). */
    void shutdown() {
        pool.shutdownNow();
    }
}
