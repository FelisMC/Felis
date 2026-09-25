package best.lolicon.felis.velocity;

import best.lolicon.felis.link.OpLoginView;
import net.kyori.adventure.text.Component;
import net.kyori.adventure.text.event.ClickEvent;
import net.kyori.adventure.text.event.HoverEvent;
import net.kyori.adventure.text.format.NamedTextColor;

import java.util.ArrayList;
import java.util.List;

/**
 * OpApprovalCard builds what an admin sees after {@code /felis web op approve <code>}:
 * whose op.console sign-in the code belongs to (account, address, where and when it
 * was started), a warning to approve only a sign-in they know about, and how to
 * confirm. Confirming means typing the account name, because a code relayed by
 * someone else ("please approve abc123") is exactly the case this card exists to
 * catch; the button fills the command up to the name and leaves the name to the
 * admin.
 *
 * <p>A pure function of (request, code, language, age), like {@link InviteCard}, so
 * {@code OpApprovalCardTest} can check it without a proxy.
 */
final class OpApprovalCard {

    static final String APPROVE_COMMAND = "/felis web op approve";

    /** A user agent longer than this is cut, so one line of chat stays one line. */
    static final int USER_AGENT_MAX = 80;

    private OpApprovalCard() {
    }

    /**
     * lines renders the card in the approver's language. ageSeconds is how long ago the
     * sign-in was started, or a negative value when the API did not say.
     */
    static List<Component> lines(OpLoginView req, String code, boolean zh, long ageSeconds) {
        List<Component> out = new ArrayList<>();
        out.add(Component.text(zh ? "待你批准的管理员登录" : "Operator sign-in waiting for your approval",
                NamedTextColor.AQUA));
        out.add(field(zh ? "账户" : "account", req.username() + "  (" + req.email() + ")"));
        String from = req.clientIp().isEmpty() ? (zh ? "未知" : "unknown") : req.clientIp();
        if (!req.userAgent().isEmpty()) {
            from += " · " + shorten(req.userAgent());
        }
        out.add(field(zh ? "来源" : "from", from));
        if (ageSeconds >= 0) {
            out.add(field(zh ? "发起" : "started", age(ageSeconds, zh)));
        }
        out.add(Component.text(
                zh ? "  只在你确认此人正在登录时批准。有人私下发给你批准码时尤其要核对账户。"
                   : "  Approve only if you know this person is signing in right now — "
                     + "above all when someone else sent you the code.",
                NamedTextColor.YELLOW));

        String prefill = APPROVE_COMMAND + " " + code + " ";
        Component button = Component.text(zh ? "[ 批准… ]" : "[ Approve… ]", NamedTextColor.GREEN)
                .clickEvent(ClickEvent.suggestCommand(prefill))
                .hoverEvent(HoverEvent.showText(Component.text(
                        zh ? "点击后在末尾输入上面的账户名，再按回车"
                           : "Click, then type the account name shown above and press Enter")));
        out.add(Component.text("  ").append(button).append(Component.text(
                zh ? "  或输入 " + APPROVE_COMMAND + " " + code + " <账户名>"
                   : "  or type " + APPROVE_COMMAND + " " + code + " <account name>",
                NamedTextColor.GRAY)));
        return out;
    }

    static String shorten(String userAgent) {
        if (userAgent.length() <= USER_AGENT_MAX) {
            return userAgent;
        }
        return userAgent.substring(0, USER_AGENT_MAX - 1) + "…";
    }

    static String age(long seconds, boolean zh) {
        if (seconds < 60) {
            return zh ? "刚刚" : "just now";
        }
        long minutes = seconds / 60;
        return zh ? minutes + " 分钟前" : minutes + " min ago";
    }

    private static Component field(String key, String value) {
        return Component.text("  " + key + ": ", NamedTextColor.GRAY)
                .append(Component.text(value, NamedTextColor.WHITE));
    }
}
