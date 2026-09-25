package best.lolicon.felis.velocity;

import java.util.concurrent.ArrayBlockingQueue;
import java.util.concurrent.RejectedExecutionException;
import java.util.concurrent.ThreadPoolExecutor;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.function.Consumer;

/**
 * BoundedExecutor runs the plugin's felis-api calls on a fixed number of threads with
 * a bounded wait queue. Velocity's scheduler starts a thread for every task it is
 * handed, so while felis-api was slow a burst of joins, menu clicks and commands
 * stacked blocking calls without limit, and each of them landed on the API the moment
 * it recovered. Here at most {@code threads} calls run and at most {@code queue} wait;
 * past that {@link #submit} refuses at once, so the caller can say "busy" instead of
 * leaving the player waiting on a call that would run minutes later.
 *
 * <p>A task that throws is handed to {@code onError} and the worker carries on, so one
 * bad task never shrinks the pool or disappears without a log line.
 */
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
