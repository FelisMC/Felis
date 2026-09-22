package best.lolicon.felis.velocity;

import java.util.Map;
import java.util.UUID;
import java.util.concurrent.ConcurrentHashMap;

/**
 * InviteBook holds the live {@code /invite} prompts: at most one per invitee, each
 * lapsing on its own deadline. It is proxy-local and deliberately unpersisted — an
 * invite is a chat prompt, not state worth surviving a restart.
 *
 * <p>An entry stores only who asked and which server they were on. Nothing here is an
 * authority: accepting runs the same {@code /felis go} path on the accepting player's
 * own verified uuid, so the worst a lapsed-but-honoured invite could do is offer a
 * player a server they were already allowed to reach. The deadline exists so a stale
 * button doesn't move someone who wandered off ten minutes ago, not to protect access.
 *
 * <p>It also holds the send-side cooldown, because the one capability {@code /invite}
 * genuinely adds is "make a chat card appear on any online player" — unrated, that is a
 * way to follow someone around their own chat log. The stamps live here rather than in a
 * second class so they are pruned by the same pass that prunes the prompts.
 *
 * <p>It takes {@code now} as a parameter rather than reading the clock itself, which is
 * what lets {@link InviteBookTest} check expiry and cooldown without sleeping. No
 * Velocity types appear here for the same reason.
 */
final class InviteBook {
    private final Map<UUID, Invite> pending = new ConcurrentHashMap<>();
    private final Map<UUID, Long> lastSent = new ConcurrentHashMap<>();
    private final long ttlMillis;
    private final long cooldownMillis;

    InviteBook(long ttlMillis, long cooldownMillis) {
        this.ttlMillis = ttlMillis;
        this.cooldownMillis = cooldownMillis;
    }

    /**
     * put records a fresh prompt for invitee, replacing any prompt they had not yet
     * answered — the newest invite is the one a click should honour. Lapsed entries for
     * everyone else are dropped in the same pass, so a map holding one entry per player
     * with a live prompt never needs a sweeper task of its own.
     *
     * <p>It also starts the sender's cooldown. Charging it HERE rather than at the top of
     * the command is what keeps a refused invite free: an offline name or a player already
     * on the server sends no card to anybody, so it costs the sender nothing.
     */
    void put(UUID invitee, UUID from, String server, long now) {
        pending.values().removeIf(i -> i.expiresAt() <= now);
        pending.put(invitee, new Invite(from, server, now + ttlMillis));
        lastSent.values().removeIf(t -> t + cooldownMillis <= now);
        lastSent.put(from, now);
    }

    /**
     * cooldownRemaining is how long the sender must still wait, in millis, or 0 when they
     * may send now.
     *
     * <p>One global stamp per sender, so inviting Alex also holds off inviting
     * Steve. That is the shape that actually stops the spam — a per-(sender, invitee) key
     * would let one sender paper every player on the proxy at once, which is the thing
     * being rate-limited. Key it per pair only if a real group of players complains.
     */
    long cooldownRemaining(UUID from, long now) {
        Long last = lastSent.get(from);
        if (last == null) {
            return 0L;
        }
        long remaining = last + cooldownMillis - now;
        return remaining > 0L ? remaining : 0L;
    }

    /**
     * take consumes the invitee's prompt and returns it, or null when they have none or
     * theirs has lapsed. Consuming either way is the point: a single answer, so a
     * double-click cannot queue two transfers.
     */
    Invite take(UUID invitee, long now) {
        Invite invite = pending.remove(invitee);
        return invite == null || invite.expiresAt() <= now ? null : invite;
    }

    /**
     * peek reads the invitee's live prompt without consuming it, so a click can be checked
     * against what is actually pending before it is spent. A card that names a superseded
     * server must be refused WITHOUT burning the invite the player still holds.
     */
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
