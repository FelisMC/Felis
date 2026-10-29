package best.lolicon.felis.paper;

import best.lolicon.felis.link.ControlFrame;

import java.util.HashMap;
import java.util.List;
import java.util.Map;

final class MenuTiles {

    /** What a click on the tile does. */
    enum Kind { CLAIM, JOIN, WAKE, LOCKED }

    /** One tile's content; {@code reason} holds one lore line each for a locked tile. */
    record Tile(Kind kind, String action, boolean mine, List<String> reason) {
    }

    // Verdicts returned by felis-api menu-access.
    static final String OWNER = "owner";
    static final String RETIRING = "retiring";
    static final String START_FAILED = "start_failed";
    static final String OWNER_ONLY = "owner_only";
    static final String ALLOWLIST = "allowlist";

    private static final String PHASE_HIBERNATED = "Hibernated";

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
        if (PHASE_HIBERNATED.equals(status.phase())) {
            return new Tile(Kind.WAKE, zh ? "唤醒" : "Resume", mine, List.of());
        }
        return new Tile(Kind.WAKE, zh ? "启动" : "Start", mine, List.of());
    }

    /** Returns the frame a click sends, or null for a locked tile (see {@link #chatReason}). */
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

    /** Returns a locked tile's reason as one sentence. */
    static String chatReason(Tile tile, boolean zh) {
        return zh ? String.join("，", tile.reason()) + "。" : String.join(". ", tile.reason()) + ".";
    }

    // Empty when the verdict does not refuse the wake.
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

    /** Returns the lifecycle phase in the player's language. */
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
            case "Hibernating":
                return zh ? "正在休眠" : "Hibernating";
            case "Hibernated":
                return zh ? "已休眠" : "Hibernated";
            default:
                return zh ? "未知" : "Unknown";
        }
    }

    /** Maps a ListUpdate's verdicts to server → verdict, skipping servers with none. */
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
