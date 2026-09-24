package best.lolicon.felis.velocity;

import best.lolicon.felis.link.LinkException;

import java.util.Map;
import java.util.UUID;
import java.util.concurrent.ConcurrentHashMap;
import java.util.function.LongSupplier;

/**
 * LinkGate answers "is this verified UUID linked to a Felis account?" for every
 * routing decision, and keeps answering during a short felis-api outage.
 *
 * <p>felis-api is asked every time; its answer always wins when it gives one, so an
 * unlink or a disabled account takes effect on the next check. Only when the call
 * fails in a way that says nothing about the account — a transport error or a 5xx —
 * does the gate fall back to a positive answer it received for the same UUID within
 * {@code graceMillis}. Without that fallback every felis-api restart turned into
 * "login verification is temporarily unavailable" for everyone leaving the login gate
 * at that moment. A UUID never seen linked, a 4xx, or a positive answer older than
 * the grace still fails closed.
 *
 * <p>Records outlive a disconnect on purpose: a player who drops during an outage and
 * reconnects is exactly the case the grace exists for. {@link #prune} bounds the map
 * to the UUIDs seen linked within the grace.
 */
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

    /**
     * check asks felis-api and records a positive answer. It rethrows the
     * LinkException when the fallback does not apply, so callers keep their existing
     * fail-closed branch for that case.
     */
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
