package best.lolicon.felis.velocity;

import best.lolicon.felis.link.ApiStats;

import java.util.concurrent.atomic.AtomicLongArray;

/**
 * ProxyStats counts the proxy-side failures that each already get a warning line, so
 * an operator can see their rate without grepping: calls the bounded pool refused,
 * join-events that failed or were dropped (each one leaves the reaper blind to a real
 * join and skips an allowlist append), transfers that did not connect, and server-list
 * refreshes that failed. {@link #window()} returns what changed since the previous
 * window; {@link #line} folds that and felis-api's own window into the periodic log
 * line.
 */
final class ProxyStats {
    enum Event {
        POOL_REFUSED("busy refusals"),
        JOIN_EVENT_FAILED("join-events failed"),
        JOIN_EVENT_DROPPED("join-events dropped"),
        TRANSFER_FAILED("transfers failed"),
        REFRESH_FAILED("server-list refreshes failed");

        final String label;

        Event(String label) {
            this.label = label;
        }
    }

    private static final Event[] EVENTS = Event.values();
    private final AtomicLongArray counts = new AtomicLongArray(EVENTS.length);
    private final long[] lastWindow = new long[EVENTS.length];

    void count(Event e) {
        counts.incrementAndGet(e.ordinal());
    }

    long total(Event e) {
        return counts.get(e.ordinal());
    }

    /** window returns each event's count since the previous call, indexed by ordinal. */
    synchronized long[] window() {
        long[] delta = new long[EVENTS.length];
        for (int i = 0; i < EVENTS.length; i++) {
            long now = counts.get(i);
            delta[i] = now - lastWindow[i];
            lastWindow[i] = now;
        }
        return delta;
    }

    /**
     * line is the periodic health line for one window, or null when the window saw no
     * felis-api call, no failure and nobody waiting (an idle proxy logs nothing).
     */
    static String line(long minutes, ApiStats.Snapshot api, long[] events, int waiting) {
        boolean any = api.calls > 0 || waiting > 0;
        StringBuilder sb = new StringBuilder();
        for (Event e : EVENTS) {
            long n = events[e.ordinal()];
            any |= n > 0;
            sb.append(", ").append(e.label).append('=').append(n);
        }
        if (!any) {
            return null;
        }
        return "Felis: last " + minutes + " min: felis-api " + api.summary() + sb + ", waiting now=" + waiting;
    }
}
