package best.lolicon.felis.paper;

import best.lolicon.felis.link.ControlFrame;

import java.util.HashMap;
import java.util.List;
import java.util.Map;

/**
 * MenuTiles decides what a {@code /menu} tile says and what clicking it does, from the
 * server's live status ({@code StatusUpdate}) and felis-api's verdict for this player
 * (the {@code ListUpdate}'s access list). It is the whole of the menu's judgement, kept
 * free of Bukkit so it can be tested as plain values; {@link FelisPaperPlugin} only
 * turns a {@link Tile} into an item.
 *
 * <p>The order follows the proxy's wake. A server that is up is open to every linked
 * player, so it is Join whatever the verdict. An ownerless one is Claim &amp; Start:
 * claiming makes the player its owner before the wake. Past those, a verdict that
 * refuses the wake locks the tile and names the reason, so nobody queues for a start
 * that cannot happen. Everything else is Wake, including a tile with no verdict (the
 * proxy could not ask felis-api), which the wake then judges as before.
 */
final class MenuTiles {

    /** Kind is what a click on the tile does. */
    enum Kind { CLAIM, JOIN, WAKE, LOCKED }

    /**
     * Tile is one tile's content: its kind, the action word in its title, whether it is
     * the player's own server, and for a locked tile the reason, one lore line each.
     */
    record Tile(Kind kind, String action, boolean mine, List<String> reason) {
    }

    // The verdicts felis-api's menu-access answers (handleInternalMenuAccess).
    static final String OWNER = "owner";
    static final String RETIRING = "retiring";
    static final String START_FAILED = "start_failed";
    static final String OWNER_ONLY = "owner_only";
    static final String ALLOWLIST = "allowlist";

    private MenuTiles() {
    }

    static Tile tile(ControlFrame status, String verdict, boolean zh) {
        boolean mine = OWNER.equals(verdict);
        if (status.ready()) {
            return new Tile(Kind.JOIN, zh ? "加入" : "Join", mine, List.of());
        }
        if (status.claimable()) {
            return new Tile(Kind.CLAIM, zh ? "认领并启动" : "Claim & Start", false, List.of());
        }
        List<String> reason = lockReason(verdict, zh);
        if (!reason.isEmpty()) {
            return new Tile(Kind.LOCKED, zh ? "无法启动" : "Can't start", mine, reason);
        }
        return new Tile(Kind.WAKE, zh ? "启动" : "Start", mine, List.of());
    }

    /**
     * click is the frame a click on the tile sends, or null for a locked tile, which
     * sends nothing and tells the player why ({@link #chatReason}).
     */
    static ControlFrame click(Tile tile, String player, String server) {
        switch (tile.kind()) {
            case CLAIM:
                return ControlFrame.claimRequest(player, server);
            case LOCKED:
                return null;
            default:
                // Join and Start are both a wake: the proxy joins a server that is up.
                return ControlFrame.wakeRequest(player, server);
        }
    }

    /** chatReason is a locked tile's reason as one sentence. */
    static String chatReason(Tile tile, boolean zh) {
        return zh ? String.join("，", tile.reason()) + "。" : String.join(". ", tile.reason()) + ".";
    }

    // lockReason is why a verdict refuses the wake, or empty when it does not.
    private static List<String> lockReason(String verdict, boolean zh) {
        if (verdict == null) {
            return List.of();
        }
        switch (verdict) {
            case RETIRING:
                return zh ? List.of("这台服务器已被放弃或正在删除", "不能再启动")
                          : List.of("This server was given up or is being deleted", "It can't be started");
            case START_FAILED:
                return zh ? List.of("多次启动失败，已停止自动重试", "需要在网页控制台处理")
                          : List.of("It failed to start several times", "It needs a fix on the web console");
            case OWNER_ONLY:
                return zh ? List.of("只有主人能启动这台服务器", "它运行时任何人都能加入")
                          : List.of("Only its owner can start it", "Anyone can join while it's running");
            case ALLOWLIST:
                return zh ? List.of("你不在这台服务器的启动名单上", "它运行时任何人都能加入")
                          : List.of("You're not on its start list", "Anyone can join while it's running");
            default:
                return List.of();
        }
    }

    /** phase is the tile's status line: the lifecycle phase in the player's language. */
    static String phase(String phase, boolean zh) {
        if (phase == null) {
            return zh ? "未知" : "Unknown";
        }
        switch (phase) {
            case "Running":
                return zh ? "运行中" : "Running";
            case "Starting":
                return zh ? "启动中" : "Starting";
            case "Stopping":
                return zh ? "停止中" : "Stopping";
            case "Stopped":
                return zh ? "已停止" : "Stopped";
            case "Failed":
                return zh ? "启动失败" : "Failed to start";
            default:
                return zh ? "未知" : "Unknown";
        }
    }

    /**
     * accessByName reads a {@code ListUpdate}'s verdicts into server → verdict, leaving
     * out the servers it gave none for (and every server when the proxy sent names only).
     */
    static Map<String, String> accessByName(ControlFrame list) {
        Map<String, String> out = new HashMap<>();
        List<String> names = list.servers();
        List<String> access = list.access();
        for (int i = 0; i < names.size() && i < access.size(); i++) {
            if (!access.get(i).isEmpty()) {
                out.put(names.get(i), access.get(i));
            }
        }
        return out;
    }
}
