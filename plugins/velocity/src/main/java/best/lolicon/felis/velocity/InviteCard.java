package best.lolicon.felis.velocity;

import net.kyori.adventure.text.Component;
import net.kyori.adventure.text.event.ClickEvent;
import net.kyori.adventure.text.event.HoverEvent;
import net.kyori.adventure.text.format.NamedTextColor;

import java.util.List;

final class InviteCard {

    static final String ACCEPT_COMMAND = "/invite accept";
    static final String DENY_COMMAND = "/invite deny";

    private InviteCard() {
    }

    /** lines renders the prompt in the invitee's language. The buttons carry a hover tip as well as the click, so a player who has clicks disabled still sees the command to type. */
    static List<Component> lines(String inviterName, String server, boolean zh, long ttlSeconds) {
        Component headline = Component.text(
                zh ? inviterName + " 邀请你前往「" + server + "」服务器"
                   : inviterName + " invites you to « " + server + " »",
                NamedTextColor.AQUA);

        // Include the server in each button command so old chat cards cannot accept unrelated current invites.
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
