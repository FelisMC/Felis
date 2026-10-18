package best.lolicon.felis.velocity;

import java.util.Map;
import java.util.UUID;
import java.util.concurrent.ConcurrentHashMap;

// Invites grant no authority; verified acceptance applies a sender-wide cooldown and injected-clock expiry.
final class InviteBook {
    private final Map<UUID, Invite> pending = new ConcurrentHashMap<>();
    private final Map<UUID, Long> lastSent = new ConcurrentHashMap<>();
    private final long ttlMillis;
    private final long cooldownMillis;

    InviteBook(long ttlMillis, long cooldownMillis) {
        this.ttlMillis = ttlMillis;
        this.cooldownMillis = cooldownMillis;
    }

    // Start cooldown only after storing an admitted invite; prune expired entries in the same pass.
    void put(UUID invitee, UUID from, String server, long now) {
        pending.values().removeIf(i -> i.expiresAt() <= now);
        pending.put(invitee, new Invite(from, server, now + ttlMillis));
        lastSent.values().removeIf(t -> t + cooldownMillis <= now);
        lastSent.put(from, now);
    }

    // Cooldown is per sender across all recipients.
    long cooldownRemaining(UUID from, long now) {
        Long last = lastSent.get(from);
        if (last == null) {
            return 0L;
        }
        long remaining = last + cooldownMillis - now;
        return remaining > 0L ? remaining : 0L;
    }

    // Consume invites once so double-clicks cannot queue duplicate transfers.
    Invite take(UUID invitee, long now) {
        Invite invite = pending.remove(invitee);
        return invite == null || invite.expiresAt() <= now ? null : invite;
    }

    // Check superseded cards without consuming the live invite.
    Invite peek(UUID invitee, long now) {
        Invite invite = pending.get(invitee);
        return invite == null || invite.expiresAt() <= now ? null : invite;
    }

    /** size is the number of entries still held, lapsed ones included. Visible for the test. */
    int size() {
        return pending.size();
    }

    /** cooldownSize is the number of send stamps still held. Visible for the test. */
    int cooldownSize() {
        return lastSent.size();
    }

    /** Invite is one live prompt: who sent it, the server they were on, when it lapses. */
    record Invite(UUID from, String server, long expiresAt) {}
}
