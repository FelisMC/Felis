package best.lolicon.felis.limbo;

import net.kyori.adventure.inventory.Book;
import net.kyori.adventure.text.Component;
import net.kyori.adventure.text.event.ClickEvent;
import net.kyori.adventure.text.format.NamedTextColor;

/**
 * LoginBook is the book the gate opens on join: the bind code, a line that opens the
 * login page when clicked, and the reminder to use the system browser, where
 * WebAuthn/passkey works (the WeChat/QQ in-app browsers break it).
 */
final class LoginBook {

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
}
