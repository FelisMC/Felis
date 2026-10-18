package best.lolicon.felis.velocity;

import best.lolicon.felis.link.LinkException;

import java.util.Map;
import java.util.UUID;
import java.util.concurrent.ConcurrentHashMap;
import java.util.function.LongSupplier;

// Fresh API answers always override cached link status, including unlinking.
// Use recent positive cache entries only for transport/5xx failures; unknown UUIDs and 4xx fail closed.
final class LinkGate {

    /** LinkCheck is the one felis-api call the gate wraps (FelisApiClient::linkStatus). */
    interface LinkCheck {
        boolean linkStatus(UUID id) throws LinkException;
    }

    /** Result is the gate's answer plus whether it came from the fallback. */
    static final class Result {
        final boolean linked;
        final boolean degraded;

        Result(boolean linked, boolean degraded) {
            this.linked = linked;
            this.degraded = degraded;
        }
    }

    private final LinkCheck api;
    private final long graceMillis;
    private final LongSupplier clock;
    private final Map<UUID, Long> lastPositive = new ConcurrentHashMap<>();

    LinkGate(LinkCheck api, long graceMillis, LongSupplier clock) {
        this.api = api;
        this.graceMillis = graceMillis;
        this.clock = clock;
    }

    /** check asks felis-api and records a positive answer. It rethrows the LinkException when the fallback does not apply, so callers keep their fail-closed branch. */
    Result check(UUID id) throws LinkException {
        try {
            boolean linked = api.linkStatus(id);
            if (linked) {
                lastPositive.put(id, clock.getAsLong());
            } else {
                lastPositive.remove(id);
            }
            return new Result(linked, false);
        } catch (LinkException e) {
            if (!outage(e)) {
                throw e;
            }
            Long at = lastPositive.get(id);
            if (at != null && clock.getAsLong() - at <= graceMillis) {
                return new Result(true, true);
            }
            throw e;
        }
    }

    /** prune drops records older than the grace; the plugin calls it on its refresh loop. */
    void prune() {
        long cutoff = clock.getAsLong() - graceMillis;
        lastPositive.values().removeIf(at -> at < cutoff);
    }

    // A transport failure or a server-side error says nothing about the account; a
    // 4xx is felis-api answering, and its answer stands.
    private static boolean outage(LinkException e) {
        return e.statusCode() == 0 || e.statusCode() >= 500;
    }
}
