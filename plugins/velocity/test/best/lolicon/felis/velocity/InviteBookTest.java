package best.lolicon.felis.velocity;

import java.util.UUID;

public final class InviteBookTest {

    private static final long TTL = 120_000L;
    private static final long COOLDOWN = 30_000L;
    private static int checks;

    public static void main(String[] args) {
        takesBackWhatWasPut();
        lapsedInviteIsNotHonoured();
        oneAnswerConsumesThePrompt();
        secondInviteSupersedesTheFirst();
        putPrunesEveryoneElsesLapsedEntries();
        strangerHasNothingToTake();
        peekLooksWithoutSpending();
        cooldownStartsOnSendAndRunsOut();
        cooldownIsPerSenderNotPerInvitee();
        spentCooldownsArePruned();
        System.out.println("InviteBookTest OK (" + checks + " checks)");
    }

    // The happy path: what the inviter offered is what the invitee gets back.
    private static void takesBackWhatWasPut() {
        InviteBook book = new InviteBook(TTL, COOLDOWN);
        UUID invitee = UUID.randomUUID();
        UUID from = UUID.randomUUID();
        book.put(invitee, from, "survival", 1_000L);
        InviteBook.Invite got = book.take(invitee, 1_000L);
        assertNotNull("invite present", got);
        assertEq("from", from, got.from());
        assertEq("server", "survival", got.server());
        assertEq("expiresAt", 1_000L + TTL, got.expiresAt());
    }

    // A prompt that has run out must not move anyone, and the boundary counts as
    // lapsed: at exactly expiresAt the button is dead.
    private static void lapsedInviteIsNotHonoured() {
        InviteBook book = new InviteBook(TTL, COOLDOWN);
        UUID invitee = UUID.randomUUID();
        book.put(invitee, UUID.randomUUID(), "survival", 0L);
        assertNull("at the deadline", book.take(invitee, TTL));

        book.put(invitee, UUID.randomUUID(), "survival", 0L);
        assertNull("past the deadline", book.take(invitee, TTL + 1));

        book.put(invitee, UUID.randomUUID(), "survival", 0L);
        assertNotNull("one milli before", book.take(invitee, TTL - 1));
    }

    // Answering consumes the prompt whether or not it was honoured, so a player who
    // clicks Accept twice cannot queue two transfers.
    private static void oneAnswerConsumesThePrompt() {
        InviteBook book = new InviteBook(TTL, COOLDOWN);
        UUID invitee = UUID.randomUUID();
        book.put(invitee, UUID.randomUUID(), "survival", 0L);
        assertNotNull("first click", book.take(invitee, 1L));
        assertNull("second click", book.take(invitee, 1L));
        assertEq("nothing left held", 0, book.size());

        // And a lapsed take clears it too, so it can't be revived by an earlier clock.
        book.put(invitee, UUID.randomUUID(), "survival", 0L);
        assertNull("lapsed click", book.take(invitee, TTL + 1));
        assertEq("lapsed take still consumed", 0, book.size());
    }

    // Two invites for the same player leave one live prompt — the newest — so the
    // buttons in chat can't disagree about where Accept sends them.
    private static void secondInviteSupersedesTheFirst() {
        InviteBook book = new InviteBook(TTL, COOLDOWN);
        UUID invitee = UUID.randomUUID();
        book.put(invitee, UUID.randomUUID(), "survival", 0L);
        book.put(invitee, UUID.randomUUID(), "creative", 10L);
        assertEq("only one held", 1, book.size());
        InviteBook.Invite got = book.take(invitee, 10L);
        assertNotNull("invite present", got);
        assertEq("newest wins", "creative", got.server());
    }

    // Prompts nobody ever answers are dropped by the next put, so the map tracks live
    // prompts rather than growing for the life of the proxy.
    private static void putPrunesEveryoneElsesLapsedEntries() {
        InviteBook book = new InviteBook(TTL, COOLDOWN);
        for (int i = 0; i < 5; i++) {
            book.put(UUID.randomUUID(), UUID.randomUUID(), "survival", 0L);
        }
        assertEq("five held", 5, book.size());
        UUID fresh = UUID.randomUUID();
        book.put(fresh, UUID.randomUUID(), "creative", TTL + 1);
        assertEq("lapsed swept, fresh kept", 1, book.size());
        assertNotNull("the fresh one survived", book.take(fresh, TTL + 1));
    }

    // Someone who was never invited has nothing to take — no entry, no crash.
    private static void strangerHasNothingToTake() {
        InviteBook book = new InviteBook(TTL, COOLDOWN);
        assertNull("never invited", book.take(UUID.randomUUID(), 0L));
    }

    // Superseded cards must not consume the current invite; expiration still applies.
    private static void peekLooksWithoutSpending() {
        InviteBook book = new InviteBook(TTL, COOLDOWN);
        UUID invitee = UUID.randomUUID();
        book.put(invitee, UUID.randomUUID(), "survival", 0L);
        assertEq("peek reads it", "survival", book.peek(invitee, 1L).server());
        assertEq("peek reads it twice", "survival", book.peek(invitee, 1L).server());
        assertNotNull("and it is still spendable", book.take(invitee, 1L));
        assertNull("gone once spent", book.peek(invitee, 1L));

        book.put(invitee, UUID.randomUUID(), "survival", 0L);
        assertNull("lapsed is not visible", book.peek(invitee, TTL + 1));
        assertNull("stranger has nothing to peek at", book.peek(UUID.randomUUID(), 0L));
    }

    // The cooldown is the only limit on making a chat card appear on any player, so its boundary is asserted: it must be charged by the send, expire, and not fire one tick early.
    private static void cooldownStartsOnSendAndRunsOut() {
        InviteBook book = new InviteBook(TTL, COOLDOWN);
        UUID from = UUID.randomUUID();
        assertEq("a sender who never sent is free", 0L, book.cooldownRemaining(from, 0L));

        book.put(UUID.randomUUID(), from, "survival", 0L);
        assertEq("charged in full on send", COOLDOWN, book.cooldownRemaining(from, 0L));
        assertEq("counts down", 1L, book.cooldownRemaining(from, COOLDOWN - 1));
        assertEq("free at the deadline", 0L, book.cooldownRemaining(from, COOLDOWN));
        assertEq("and stays free after", 0L, book.cooldownRemaining(from, COOLDOWN + 5_000));

        // Sending again re-arms it, so it is a rate limit and not a one-off toll.
        book.put(UUID.randomUUID(), from, "survival", COOLDOWN);
        assertEq("re-armed by the next send", COOLDOWN, book.cooldownRemaining(from, COOLDOWN));
    }

    // Cooldown spans all recipients for one sender without affecting other senders.
    private static void cooldownIsPerSenderNotPerInvitee() {
        InviteBook book = new InviteBook(TTL, COOLDOWN);
        UUID alex = UUID.randomUUID();
        UUID steve = UUID.randomUUID();
        book.put(UUID.randomUUID(), alex, "survival", 0L);
        assertEq("held off for a different invitee too", COOLDOWN, book.cooldownRemaining(alex, 0L));
        assertEq("someone else is unaffected", 0L, book.cooldownRemaining(steve, 0L));
    }

    // Stamps are swept by the next send like the prompts are, so the map tracks senders on
    // cooldown rather than every sender the proxy has ever seen.
    private static void spentCooldownsArePruned() {
        InviteBook book = new InviteBook(TTL, COOLDOWN);
        for (int i = 0; i < 5; i++) {
            book.put(UUID.randomUUID(), UUID.randomUUID(), "survival", 0L);
        }
        assertEq("five stamps held", 5, book.cooldownSize());
        UUID fresh = UUID.randomUUID();
        book.put(UUID.randomUUID(), fresh, "creative", COOLDOWN);
        assertEq("spent stamps swept, the new one kept", 1, book.cooldownSize());
        assertEq("and it is the live one", COOLDOWN, book.cooldownRemaining(fresh, COOLDOWN));
    }


    private static void assertEq(String what, Object want, Object got) {
        if (want == null ? got != null : !want.equals(got)) {
            throw new AssertionError(what + " = " + got + ", want " + want);
        }
        checks++;
    }

    private static void assertNull(String what, Object got) {
        if (got != null) {
            throw new AssertionError(what + " = " + got + ", want null");
        }
        checks++;
    }

    private static void assertNotNull(String what, Object got) {
        if (got == null) {
            throw new AssertionError(what + " = null, want an invite");
        }
        checks++;
    }
}
