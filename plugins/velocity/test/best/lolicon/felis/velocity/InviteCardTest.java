package best.lolicon.felis.velocity;

import net.kyori.adventure.text.Component;
import net.kyori.adventure.text.TextComponent;
import net.kyori.adventure.text.event.ClickEvent;
import net.kyori.adventure.text.format.NamedTextColor;
import net.kyori.adventure.text.format.TextColor;

import java.util.List;

/**
 * InviteCardTest checks the one thing the /invite feature was actually asked for: a chat
 * prompt naming who invites you where, a GREEN clickable Accept and a RED clickable Deny.
 * Colour and click target are the requirement, so they are asserted rather than eyeballed
 * — a proxy console cannot render either, and a screenshot is not a check.
 *
 * <p>Hermetic and framework-free like {@link InviteBookTest}: it lives outside
 * {@code src/main/java} so it never ships in the plugin jar, needs only adventure-api on
 * the classpath, and a failed assertion throws so the process exits non-zero.
 *
 * <p>Run: {@code javac -cp <adventure-api.jar> -d <out>
 * velocity/src/main/java/best/lolicon/felis/velocity/InviteCard.java
 * velocity/test/best/lolicon/felis/velocity/InviteCardTest.java && java -cp
 * <out>;<adventure-api.jar>;<adventure-key.jar>;<examination-api.jar>
 * best.lolicon.felis.velocity.InviteCardTest}.
 */
public final class InviteCardTest {

    private static int checks;

    public static void main(String[] args) {
        cardNamesWhoAndWhere();
        acceptIsGreenAndRunsAccept();
        denyIsRedAndRunsDeny();
        buttonsCarryAHoverTip();
        footerStatesTheDeadline();
        englishCardIsTheSameShape();
        buttonsArePinnedToTheirOwnServer();
        System.out.println("InviteCardTest OK (" + checks + " checks)");
    }

    // The headline has to answer "who wants me, and where" — both names, in the
    // invitee's language.
    private static void cardNamesWhoAndWhere() {
        List<Component> card = InviteCard.lines("Steve", "survival", true, 120);
        assertEq("three lines", 3, card.size());
        String head = plain(card.get(0));
        assertTrue("names the inviter", head.contains("Steve"));
        assertTrue("names the server", head.contains("survival"));
        assertEq("headline colour", NamedTextColor.AQUA, card.get(0).color());
    }

    // Green, clickable, and pointed at the command that actually accepts. A button of the
    // right colour wired to the wrong command is the failure this catches.
    private static void acceptIsGreenAndRunsAccept() {
        Component accept = button(InviteCard.lines("Steve", "survival", true, 120), 0);
        assertEq("accept is green", NamedTextColor.GREEN, accept.color());
        assertTrue("accept is labelled", plain(accept).contains("接受"));
        ClickEvent click = accept.clickEvent();
        assertNotNull("accept is clickable", click);
        assertEq("accept runs a command", ClickEvent.Action.RUN_COMMAND, click.action());
        // Compared whole rather than by the getter: ClickEvent#value() is deprecated in
        // adventure 4.26, and equality covers action and payload in one assertion.
        assertEq("accept target", ClickEvent.runCommand("/invite accept survival"), click);
    }

    private static void denyIsRedAndRunsDeny() {
        Component deny = button(InviteCard.lines("Steve", "survival", true, 120), 2);
        assertEq("deny is red", NamedTextColor.RED, deny.color());
        assertTrue("deny is labelled", plain(deny).contains("拒绝"));
        ClickEvent click = deny.clickEvent();
        assertNotNull("deny is clickable", click);
        assertEq("deny runs a command", ClickEvent.Action.RUN_COMMAND, click.action());
        assertEq("deny target", ClickEvent.runCommand("/invite deny survival"), click);
    }

    // A player whose client has chat clicks disabled still needs a way in, so the hover
    // spells out the command to type.
    private static void buttonsCarryAHoverTip() {
        List<Component> card = InviteCard.lines("Steve", "survival", false, 120);
        assertNotNull("accept has a tip", button(card, 0).hoverEvent());
        assertNotNull("deny has a tip", button(card, 2).hoverEvent());
        assertTrue("tip spells the command",
                plainDeep(button(card, 0).hoverEvent().value()).contains("/invite accept survival"));
    }

    private static void footerStatesTheDeadline() {
        String footer = plain(InviteCard.lines("Steve", "survival", true, 120).get(2));
        assertTrue("footer states the ttl", footer.contains("120"));
        assertEq("footer colour", NamedTextColor.GRAY,
                InviteCard.lines("Steve", "survival", true, 120).get(2).color());
    }

    // The English card is not a second implementation: same three lines, same colours,
    // same click targets, only the words change.
    private static void englishCardIsTheSameShape() {
        List<Component> card = InviteCard.lines("Alex", "creative", false, 90);
        assertEq("three lines", 3, card.size());
        assertTrue("names the inviter", plain(card.get(0)).contains("Alex"));
        assertTrue("names the server", plain(card.get(0)).contains("creative"));
        assertEq("accept is green", NamedTextColor.GREEN, button(card, 0).color());
        assertEq("accept target", ClickEvent.runCommand("/invite accept creative"), button(card, 0).clickEvent());
        assertEq("deny is red", NamedTextColor.RED, button(card, 2).color());
        assertEq("deny target", ClickEvent.runCommand("/invite deny creative"), button(card, 2).clickEvent());
        assertTrue("english labels", plain(button(card, 0)).contains("Accept"));
        assertTrue("english labels", plain(button(card, 2)).contains("Deny"));
        assertTrue("footer states the ttl", plain(card.get(2)).contains("90"));
    }

    // Chat scrollback keeps every card clickable forever while only the newest invite is
    // live, so two cards MUST NOT click to the same command — otherwise last week's button
    // silently answers today's invite and sends the player to a server they never agreed
    // to. Different server in, different click target out.
    private static void buttonsArePinnedToTheirOwnServer() {
        List<Component> old = InviteCard.lines("Steve", "survival", true, 120);
        List<Component> fresh = InviteCard.lines("Alex", "creative", true, 120);
        assertTrue("accept targets differ per server",
                !button(old, 0).clickEvent().equals(button(fresh, 0).clickEvent()));
        assertTrue("deny targets differ per server",
                !button(old, 2).clickEvent().equals(button(fresh, 2).clickEvent()));
        assertEq("the stale card still names its own server",
                ClickEvent.runCommand("/invite accept survival"), button(old, 0).clickEvent());
    }

    // ---- harness ----

    /** button pulls the nth child off the button row: 0 = Accept, 1 = spacer, 2 = Deny. */
    private static Component button(List<Component> card, int index) {
        List<Component> row = card.get(1).children();
        assertEq("button row shape", 3, row.size());
        return row.get(index);
    }

    private static String plain(Component c) {
        return c instanceof TextComponent ? ((TextComponent) c).content() : c.toString();
    }

    /** plainDeep flattens a component and its children — hover text may be nested. */
    private static String plainDeep(Object value) {
        if (!(value instanceof Component)) {
            return String.valueOf(value);
        }
        Component c = (Component) value;
        StringBuilder sb = new StringBuilder(plain(c));
        for (Component child : c.children()) {
            sb.append(plainDeep(child));
        }
        return sb.toString();
    }

    private static void assertEq(String what, Object want, Object got) {
        if (want instanceof TextColor && got instanceof TextColor) {
            if (((TextColor) want).value() != ((TextColor) got).value()) {
                throw new AssertionError(what + " = " + got + ", want " + want);
            }
            checks++;
            return;
        }
        if (want == null ? got != null : !want.equals(got)) {
            throw new AssertionError(what + " = " + got + ", want " + want);
        }
        checks++;
    }

    private static void assertTrue(String what, boolean got) {
        if (!got) {
            throw new AssertionError(what + " = false, want true");
        }
        checks++;
    }

    private static void assertNotNull(String what, Object got) {
        if (got == null) {
            throw new AssertionError(what + " = null, want a value");
        }
        checks++;
    }
}
