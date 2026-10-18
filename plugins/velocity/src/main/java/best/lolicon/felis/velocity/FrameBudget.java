package best.lolicon.felis.velocity;

import java.util.Map;
import java.util.UUID;
import java.util.concurrent.ConcurrentHashMap;
import java.util.function.LongSupplier;

// Budget menu frames per player because each may consume a blocking API call.
// Refill is continuous at refillPerSecond; the clock is injected so the arithmetic is testable (FrameBudgetTest).
final class FrameBudget {
    private final double capacity;
    private final double refillPerMilli;
    private final LongSupplier clock;
    private final Map<UUID, double[]> buckets = new ConcurrentHashMap<>(); // {tokens, lastMillis}

    FrameBudget(int capacity, double refillPerSecond, LongSupplier clock) {
        this.capacity = capacity;
        this.refillPerMilli = refillPerSecond / 1000.0;
        this.clock = clock;
    }

    /** tryTake spends one token for {@code id}; false means drop this frame. */
    boolean tryTake(UUID id) {
        long now = clock.getAsLong();
        double[] b = buckets.computeIfAbsent(id, k -> new double[] {capacity, now});
        synchronized (b) {
            double tokens = Math.min(capacity, b[0] + (now - (long) b[1]) * refillPerMilli);
            b[1] = now;
            if (tokens < 1.0) {
                b[0] = tokens;
                return false;
            }
            b[0] = tokens - 1.0;
            return true;
        }
    }

    /** forget drops a departed player's bucket so the map does not grow without bound. */
    void forget(UUID id) {
        buckets.remove(id);
    }
}
