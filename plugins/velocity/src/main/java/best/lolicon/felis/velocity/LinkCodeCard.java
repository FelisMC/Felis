package best.lolicon.felis.velocity;

import net.kyori.adventure.text.Component;
import net.kyori.adventure.text.event.ClickEvent;
import net.kyori.adventure.text.event.HoverEvent;
import net.kyori.adventure.text.format.NamedTextColor;
import net.kyori.adventure.text.format.TextDecoration;

import java.util.List;

/**
 * LinkCodeCard builds the reply to {@code /link}: the bind code, then where to enter
 * it. The panel address is a click-to-open link, because the client makes nothing in
 * server chat clickable by itself and a bare address had to be typed out by hand.
 *
 * <p>Like {@link InviteCard} it holds no Velocity types, so {@link LinkCodeCardTest}
 * can check the link without a live proxy. Sending is left to the caller.
 */
final class LinkCodeCard {

    private LinkCodeCard() {
    }

    /** lines renders the reply; panelUrl is null when felis-api minted none. */
    static List<Component> lines(String code, String panelUrl, boolean zh) {
        Component codeLine = Component.text(zh ? "你的绑定码：" : "Your link code: ", NamedTextColor.GREEN)
                .append(Component.text(code, NamedTextColor.YELLOW));
        if (panelUrl == null) {
            return List.of(codeLine, Component.text(
                    zh ? "在网页控制台 → 账户 中输入它完成绑定（几分钟内有效）。"
                       : "Enter it on the web console → Account to finish linking (valid a few minutes).",
                    NamedTextColor.GRAY));
        }
        Component link = Component.text(panelUrl, NamedTextColor.AQUA, TextDecoration.UNDERLINED)
                .clickEvent(ClickEvent.openUrl(panelUrl))
                .hoverEvent(HoverEvent.showText(Component.text(
                        zh ? "点击在浏览器中打开" : "Click to open it in your browser")));
        return List.of(codeLine,
                Component.text(zh ? "在这里输入它完成绑定（几分钟内有效）："
                                  : "Enter it here to finish linking (valid a few minutes):",
                        NamedTextColor.GRAY),
                Component.text("  ").append(link));
    }
}
