package best.lolicon.felis.velocity;

import net.kyori.adventure.text.Component;
import net.kyori.adventure.text.TextComponent;
import net.kyori.adventure.text.event.ClickEvent;
import net.kyori.adventure.text.format.TextDecoration;

import java.util.List;

/**
 * LinkCodeCardTest checks the {@code /link} reply: the code, and a panel address the
 * player can click open instead of typing it out.
 *
 * <p>Hermetic and framework-free like {@link InviteCardTest}, and run the same way:
 * {@code javac -cp <adventure-api.jar>:<adventure-key.jar>:<examination-api.jar> -d <out>
 * velocity/src/main/java/best/lolicon/felis/velocity/LinkCodeCard.java
 * velocity/test/best/lolicon/felis/velocity/LinkCodeCardTest.java && java -cp
 * <out>:<adventure-api.jar>:<adventure-key.jar>:<examination-api.jar>
 * best.lolicon.felis.velocity.LinkCodeCardTest}.
 */
public final class LinkCodeCardTest {

    private static final String URL = "https://console.example.test/link";

    private static int checks;

    public static void main(String[] args) {
        for (boolean zh : new boolean[] {true, false}) {
            List<Component> card = LinkCodeCard.lines("K7P2QX", URL, zh);
            String lang = zh ? "zh: " : "en: ";
            assertEq(lang + "three lines", 3, card.size());
            assertEq(lang + "the code line", zh ? "你的绑定码：K7P2QX" : "Your link code: K7P2QX", flat(card.get(0)));
            assertEq(lang + "the address line", "  " + URL, flat(card.get(2)));
            List<Component> links = card.get(2).children();
            assertEq(lang + "one link", 1, links.size());
            Component link = links.get(0);
            // Checked by equals alone: printing a ClickEvent needs examination-string,
            // which this classpath leaves out, so a mismatch would die in toString.
            assertTrue(lang + "the link opens the panel", ClickEvent.openUrl(URL).equals(link.clickEvent()));
            assertEq(lang + "the link looks like one", TextDecoration.State.TRUE, link.decoration(TextDecoration.UNDERLINED));
            assertTrue(lang + "the link says what a click does", link.hoverEvent() != null);
        }
        List<Component> bare = LinkCodeCard.lines("K7P2QX", null, true);
        assertEq("no panel URL: two lines", 2, bare.size());
        assertTrue("no panel URL: points at the console", flat(bare.get(1)).contains("网页控制台"));
        assertTrue("no panel URL: nothing to click", bare.get(1).clickEvent() == null);
        System.out.println("LinkCodeCardTest OK (" + checks + " checks)");
    }

    private static String flat(Component c) {
        StringBuilder sb = new StringBuilder(c instanceof TextComponent ? ((TextComponent) c).content() : "");
        for (Component child : c.children()) {
            sb.append(flat(child));
        }
        return sb.toString();
    }

    private static void assertTrue(String what, boolean ok) {
        if (!ok) {
            throw new AssertionError(what);
        }
        checks++;
    }

    private static void assertEq(String what, Object want, Object got) {
        if (want == null ? got != null : !want.equals(got)) {
            throw new AssertionError(what + " = " + got + ", want " + want);
        }
        checks++;
    }
}
