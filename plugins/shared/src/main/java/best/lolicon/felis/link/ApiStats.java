package best.lolicon.felis.link;

import java.util.concurrent.atomic.AtomicLong;

/**
 * ApiStats counts what a {@link FelisApiClient} sent and how it came back, so the
 * plugin can report felis-api's health as its callers see it: calls, the ones that
 * never got an answer (refused, reset, timed out), 4xx and 5xx answers, retries, and
 * latency. Every attempt is one call, a retried GET counting twice. Counters only
 * grow; {@link #window()} hands out what changed since the previous window, which is
 * what a periodic log line wants.
 */
public final class ApiStats {
    private final AtomicLong calls = new AtomicLong();
    private final AtomicLong transport = new AtomicLong();
    private final AtomicLong clientErrors = new AtomicLong();
    private final AtomicLong serverErrors = new AtomicLong();
    private final AtomicLong retries = new AtomicLong();
    private final AtomicLong totalMillis = new AtomicLong();
    private final AtomicLong windowMaxMillis = new AtomicLong();
    private final AtomicLong maxMillis = new AtomicLong();
    private Snapshot lastWindow = new Snapshot(0, 0, 0, 0, 0, 0, 0);

    /** record notes one attempt: its HTTP status (0 when no answer came) and how long it took. */
    void record(int status, long millis) {
        calls.incrementAndGet();
        if (status == 0) {
            transport.incrementAndGet();
        } else if (status >= 500) {
            serverErrors.incrementAndGet();
        } else if (status >= 400) {
            clientErrors.incrementAndGet();
        }
        totalMillis.addAndGet(millis);
        windowMaxMillis.accumulateAndGet(millis, Math::max);
        maxMillis.accumulateAndGet(millis, Math::max);
    }

    void retried() {
        retries.incrementAndGet();
    }

    /** total is every count since the client was made, with the slowest call ever. */
    public Snapshot total() {
        return new Snapshot(calls.get(), transport.get(), clientErrors.get(), serverErrors.get(),
                retries.get(), totalMillis.get(), maxMillis.get());
    }

    /**
     * window returns the counts since the previous call (the first call: since the
     * client was made), with the slowest call in that span, and starts a new window, so
     * consecutive windows never overlap.
     */
    public synchronized Snapshot window() {
        long max = windowMaxMillis.getAndSet(0);
        Snapshot now = new Snapshot(calls.get(), transport.get(), clientErrors.get(), serverErrors.get(),
                retries.get(), totalMillis.get(), max);
        Snapshot delta = new Snapshot(
                now.calls - lastWindow.calls,
                now.transport - lastWindow.transport,
                now.clientErrors - lastWindow.clientErrors,
                now.serverErrors - lastWindow.serverErrors,
                now.retries - lastWindow.retries,
                now.totalMillis - lastWindow.totalMillis,
                max);
        lastWindow = now;
        return delta;
    }

    /** Snapshot is one immutable reading of the counters. */
    public static final class Snapshot {
        public final long calls;
        public final long transport;
        public final long clientErrors;
        public final long serverErrors;
        public final long retries;
        public final long totalMillis;
        public final long maxMillis;

        Snapshot(long calls, long transport, long clientErrors, long serverErrors,
                 long retries, long totalMillis, long maxMillis) {
            this.calls = calls;
            this.transport = transport;
            this.clientErrors = clientErrors;
            this.serverErrors = serverErrors;
            this.retries = retries;
            this.totalMillis = totalMillis;
            this.maxMillis = maxMillis;
        }

        /** avgMillis is the mean call time, 0 with no calls. */
        public long avgMillis() {
            return calls == 0 ? 0 : totalMillis / calls;
        }

        /** summary is the one-line form the plugins log and print. */
        public String summary() {
            return "calls=" + calls + " (no answer=" + transport + ", 4xx=" + clientErrors
                    + ", 5xx=" + serverErrors + ", retried=" + retries + "), avg=" + avgMillis()
                    + " ms, max=" + maxMillis + " ms";
        }
    }
}
