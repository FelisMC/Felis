package best.lolicon.felis.limbo;

import net.kyori.adventure.inventory.Book;
import net.kyori.adventure.text.Component;
import net.kyori.adventure.text.event.ClickEvent;
import net.kyori.adventure.text.format.NamedTextColor;
import net.kyori.adventure.text.format.Style;
import net.kyori.adventure.text.format.TextDecoration;
import net.kyori.adventure.text.serializer.legacy.LegacyComponentSerializer;

import java.util.regex.Pattern;

/**
 * LoginBook renders what the gate shows a player: the book it opens on join (the bind
 * code, a line that opens the login page when clicked, and the reminder to use the
 * system browser, where WebAuthn/passkey works; the WeChat/QQ in-app browsers break
 * it), and the chat lines, whose web addresses open when clicked.
 */
final class LoginBook {

    // A web address in a chat line runs to the next space. The client makes nothing
    // in server chat clickable by itself, so a player who closed the book had to type
    // the login URL out by hand.
    private static final LegacyComponentSerializer CHAT = LegacyComponentSerializer.builder()
            .character(LegacyComponentSerializer.SECTION_CHAR)
            .extractUrls(Pattern.compile("https?://\\S+"), Style.style(NamedTextColor.AQUA, TextDecoration.UNDERLINED))
            .build();

    private LoginBook() {
    }

    static Book book(String code, String url) {
        Component page = Component.text("Felis 登录 / Login\n\n")
                .append(Component.text("绑定码 / Code:\n"))
                .append(Component.text(code + "\n\n").color(NamedTextColor.GOLD))
                .append(Component.text("▶ 点此打开登录页\n▶ Open login page\n")
                        .color(NamedTextColor.AQUA)
                        .clickEvent(ClickEvent.openUrl(url)))
                .append(Component.text("\n在系统浏览器中完成。\nUse your SYSTEM browser —\nnot WeChat / QQ (passkey\nwon't work there).")
                        .color(NamedTextColor.GRAY));
        return Book.book(Component.text("Felis Login"), Component.text("Felis"), page);
    }

    /** chatLine renders one chat line with § colour codes, its web addresses clickable. */
    static Component chatLine(String line) {
        return CHAT.deserialize(line);
    }
}
