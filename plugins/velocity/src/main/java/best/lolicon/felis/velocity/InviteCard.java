package best.lolicon.felis.velocity;

import net.kyori.adventure.text.Component;
import net.kyori.adventure.text.event.ClickEvent;
import net.kyori.adventure.text.event.HoverEvent;
import net.kyori.adventure.text.format.NamedTextColor;

import java.util.List;

/**
 * InviteCard builds the chat prompt an invited player sees: one line naming who wants
 * them where, then a green Accept and a red Deny that are real click-to-run commands,
 * so answering is a click rather than a command they have to retype.
 *
 * <p>It is a pure function of (inviter, server, language, ttl) and holds no Velocity
 * types, which is the point: the buttons are the whole feature, and this way
 * {@link InviteCardTest} can assert their colour and their click command without a live
 * proxy. Sending is left to the caller.
 */
final class InviteCard {

    static final String ACCEPT_COMMAND = "/invite accept";
    static final String DENY_COMMAND = "/invite deny";

    private InviteCard() {
    }

    /**
     * lines renders the prompt in the INVITEE's language — they are the one being asked.
     * The two buttons carry a hover tip as well as the click: a player who does not know
     * chat can be clicked finds out by pointing at it, and one who has clicks disabled at
     * least sees the command to type.
     */
    static List<Component> lines(String inviterName, String server, boolean zh, long ttlSeconds) {
        Component headline = Component.text(
                zh ? inviterName + " 邀请你前往「" + server + "」服务器"
                   : inviterName + " invites you to « " + server + " »",
                NamedTextColor.AQUA);

        // Each button names the server this card is advertising. Chat scrollback keeps old
        // cards clickable forever, and a newer invite replaces the pending one, so a bare
        // "/invite accept" clicked on last week's card would honour today's invite and send
        // the player somewhere they never agreed to. Naming it makes the click checkable.
        String accept = ACCEPT_COMMAND + " " + server;
        String deny = DENY_COMMAND + " " + server;
        Component buttons = Component.text("  ")
                .append(button(zh ? "[ 接受 ]" : "[ Accept ]", NamedTextColor.GREEN, accept,
                        zh ? "点击接受（或输入 " + accept + "）"
                           : "Click to accept (or type " + accept + ")"))
                .append(Component.text("   "))
                .append(button(zh ? "[ 拒绝 ]" : "[ Deny ]", NamedTextColor.RED, deny,
                        zh ? "点击拒绝（或输入 " + deny + "）"
                           : "Click to decline (or type " + deny + ")"));

        Component footer = Component.text(
                zh ? "  （" + ttlSeconds + " 秒内有效）"
                   : "  (valid for " + ttlSeconds + "s)",
                NamedTextColor.GRAY);

        return List.of(headline, buttons, footer);
    }

    private static Component button(String label, NamedTextColor colour, String command, String tip) {
        return Component.text(label, colour)
                .clickEvent(ClickEvent.runCommand(command))
                .hoverEvent(HoverEvent.showText(Component.text(tip)));
    }
}
