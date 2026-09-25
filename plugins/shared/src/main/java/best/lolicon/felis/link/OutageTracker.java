package best.lolicon.felis.link;

import java.util.function.LongSupplier;

/**
 * OutageTracker decides which failures of a repeated felis-api call are worth a log
 * line. A poll that runs every few seconds for every waiting player would bury the
 * log if each failure were a warning, and hides the outage entirely if each is a
 * debug line. Instead: the first failure after a success is {@link Report#DOWN}
 * (log a warning), later failures are {@link Report#STILL_DOWN} at most once per
 * {@code repeatMillis} (a reminder with the running count), the first success after
 * failures is {@link Report#RECOVERED} (log how long it lasted), and everything
 * else is {@link Report#NONE}.
 */
public final class OutageTracker {
    public enum Report { NONE, DOWN, STILL_DOWN, RECOVERED }

    private final LongSupplier clock;
    private final long repeatMillis;
    private long downSince = -1;
    private long lastReport;
    private long failures;
    private long lastOutageFailures;
    private long lastOutageMillis;

    public OutageTracker(long repeatMillis, LongSupplier clock) {
        this.repeatMillis = repeatMillis;
        this.clock = clock;
    }

    public synchronized Report failure() {
        long now = clock.getAsLong();
        failures++;
        if (downSince < 0) {
            downSince = now;
            lastReport = now;
            return Report.DOWN;
        }
        if (now - lastReport >= repeatMillis) {
            lastReport = now;
            return Report.STILL_DOWN;
        }
        return Report.NONE;
    }

    public synchronized Report success() {
        if (downSince < 0) {
            return Report.NONE;
        }
        lastOutageFailures = failures;
        lastOutageMillis = clock.getAsLong() - downSince;
        downSince = -1;
        failures = 0;
        return Report.RECOVERED;
    }

    /** failures is how many calls have failed in the current outage (0 when up). */
    public synchronized long failures() {
        return failures;
    }

    /** downForMillis is how long the current outage has lasted (0 when up). */
    public synchronized long downForMillis() {
        return downSince < 0 ? 0 : clock.getAsLong() - downSince;
    }

    /** lastOutageFailures and lastOutageMillis describe the outage a RECOVERED report just closed. */
    public synchronized long lastOutageFailures() {
        return lastOutageFailures;
    }

    public synchronized long lastOutageMillis() {
        return lastOutageMillis;
    }
}
