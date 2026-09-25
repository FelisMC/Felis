package best.lolicon.felis.velocity;

import best.lolicon.felis.link.OpLoginView;
import net.kyori.adventure.text.Component;
import net.kyori.adventure.text.TextComponent;
import net.kyori.adventure.text.event.ClickEvent;
import net.kyori.adventure.text.format.NamedTextColor;
import net.kyori.adventure.text.format.TextColor;

import java.time.Instant;
import java.util.ArrayList;
import java.util.List;

/**
 * OpApprovalCardTest checks what an admin reads before vouching for an op.console
 * sign-in: the account, its address, where and when the sign-in started, and a
 * confirm button that leaves the account name for the admin to type. The name is the
 * point — a button that filled it in would let a relayed code be approved without
 * reading the card.
 *
 * <p>Hermetic and framework-free like {@link InviteCardTest}; it needs the shared link
 * sources (for {@link OpLoginView}) and the Kyori jars. Run: {@code javac -cp
 * <adventure-api.jar>:<adventure-key.jar>:<examination-api.jar> -d <out>
 * shared/src/main/java/best/lolicon/felis/link/*.java
 * velocity/src/main/java/best/lolicon/felis/velocity/OpApprovalCard.java
 * velocity/test/best/lolicon/felis/velocity/OpApprovalCardTest.java && java -cp
 * <out>:<adventure-api.jar>:<adventure-key.jar>:<examination-api.jar>
 * best.lolicon.felis.velocity.OpApprovalCardTest}.
 */
public final class OpApprovalCardTest {

    private static int checks;

    private static final OpLoginView REQ = new OpLoginView("abc123", "alice", "alice@example.net",
            "203.0.113.9", "Mozilla/5.0 Firefox/140.0", Instant.parse("2023-11-14T22:13:20Z"));

    public static void main(String[] args) {
        cardNamesTheAccountAndOrigin();
        englishCardReadsTheSame();
        buttonLeavesTheNameToTheAdmin();
        missingOriginStillReads();
        longUserAgentIsCut();
        ageTurnsIntoMinutesAtSixty();
        System.out.println("OpApprovalCardTest OK (" + checks + " checks)");
    }

    private static void cardNamesTheAccountAndOrigin() {
        List<Component> card = OpApprovalCard.lines(REQ, "abc123", true, 150);
        assertEq("zh card", List.of(
                "待你批准的管理员登录",
                "  账户: alice  (alice@example.net)",
                "  来源: 203.0.113.9 · Mozilla/5.0 Firefox/140.0",
                "  发起: 2 分钟前",
                "  只在你确认此人正在登录时批准。有人私下发给你批准码时尤其要核对账户。",
                "  [ 批准… ]  或输入 /felis web op approve abc123 <账户名>"), text(card));
        assertEq("headline colour", NamedTextColor.AQUA, card.get(0).color());
        assertEq("warning colour", NamedTextColor.YELLOW, card.get(4).color());
    }

    private static void englishCardReadsTheSame() {
        assertEq("en card", List.of(
                "Operator sign-in waiting for your approval",
                "  account: alice  (alice@example.net)",
                "  from: 203.0.113.9 · Mozilla/5.0 Firefox/140.0",
                "  started: just now",
                "  Approve only if you know this person is signing in right now — above all when someone else sent you the code.",
                "  [ Approve… ]  or type /felis web op approve abc123 <account name>"),
                text(OpApprovalCard.lines(REQ, "abc123", false, 59)));
    }

    // The button only fills the command up to the name: clicking it must not approve,
    // and must not type the name for the admin.
    private static void buttonLeavesTheNameToTheAdmin() {
        List<Component> card = OpApprovalCard.lines(REQ, "abc123", false, 0);
        Component button = card.get(card.size() - 1).children().get(0);
        assertEq("button colour", NamedTextColor.GREEN, button.color());
        // Action and text are compared as plain values: ClickEvent#toString needs
        // examination-string, which is not on this test's classpath.
        ClickEvent click = button.clickEvent();
        assertEq("button only fills the chat box", ClickEvent.Action.SUGGEST_COMMAND, click.action());
        assertEq("button fills the command up to the name", "/felis web op approve abc123 ",
                ((ClickEvent.Payload.Text) click.payload()).value());
        assertEq("button hover", "Click, then type the account name shown above and press Enter",
                flat(button.hoverEvent().value()));
    }

    private static void missingOriginStillReads() {
        OpLoginView bare = new OpLoginView("abc123", "alice", "alice@example.net", "", "", null);
        assertEq("bare card", List.of(
                "Operator sign-in waiting for your approval",
                "  account: alice  (alice@example.net)",
                "  from: unknown",
                "  Approve only if you know this person is signing in right now — above all when someone else sent you the code.",
                "  [ Approve… ]  or type /felis web op approve abc123 <account name>"),
                text(OpApprovalCard.lines(bare, "abc123", false, -1)));
    }

    private static void longUserAgentIsCut() {
        String ua = "x".repeat(200);
        OpLoginView req = new OpLoginView("abc123", "alice", "alice@example.net", "203.0.113.9", ua, null);
        assertEq("from line", "  from: 203.0.113.9 · " + "x".repeat(79) + "…",
                text(OpApprovalCard.lines(req, "abc123", false, -1)).get(2));
        assertEq("short agent kept", "Firefox/140.0", OpApprovalCard.shorten("Firefox/140.0"));
        assertEq("80 chars kept", "y".repeat(80), OpApprovalCard.shorten("y".repeat(80)));
    }

    private static void ageTurnsIntoMinutesAtSixty() {
        assertEq("59s", "just now", OpApprovalCard.age(59, false));
        assertEq("60s", "1 min ago", OpApprovalCard.age(60, false));
        assertEq("599s", "9 分钟前", OpApprovalCard.age(599, true));
    }

    // ---- harness ----

    private static List<String> text(List<Component> card) {
        List<String> out = new ArrayList<>();
        for (Component c : card) {
            out.add(flat(c));
        }
        return out;
    }

    /** flat joins a component's text with its children's, depth first. */
    private static String flat(Object value) {
        if (!(value instanceof Component)) {
            return String.valueOf(value);
        }
        Component c = (Component) value;
        StringBuilder sb = new StringBuilder(c instanceof TextComponent ? ((TextComponent) c).content() : "");
        for (Component child : c.children()) {
            sb.append(flat(child));
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
}
