package best.lolicon.felis.paper;

import best.lolicon.felis.link.Control;
import best.lolicon.felis.link.ControlFrame;

import java.util.ArrayList;
import java.util.Arrays;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

/**
 * MenuTilesTest checks the menu's judgement: which tile a server gets from its live
 * status and felis-api's verdict for the player, what a click on it sends, the reason
 * a grey tile gives, the phase in each language, and that the verdicts survive the
 * wire and the paging. Framework free: a failed assertion throws.
 *
 * <p>Run: {@code ./gradlew menuTilesTest} (or {@code lobbyTest}, which runs both mains)
 * in plugins/paper.
 */
public final class MenuTilesTest {

    private static int checks;

    // Every verdict felis-api answers, then none, an empty one, and one from a newer felis-api.
    private static final List<String> VERDICTS = Arrays.asList(
            "owner", "wake", "owner_only", "allowlist", "retiring", "start_failed", null, "", "future_verdict");

    public static void main(String[] args) {
        upServers();
        ownerless();
        stopped();
        clicks();
        phases();
        wire();
        System.out.println("MenuTilesTest OK (" + checks + " checks)");
    }

    // An up server is open to every linked player: Join whatever the verdict, ownerless or not.
    private static void upServers() {
        for (String v : VERDICTS) {
            for (boolean claimable : new boolean[] {false, true}) {
                MenuTiles.Tile t = MenuTiles.tile(status(true, claimable), v, true);
                assertEq("up, " + v + ", claimable=" + claimable, "JOIN 加入 " + "owner".equals(v) + " []", brief(t));
            }
        }
        assertEq("up, English", "JOIN Join true []", brief(MenuTiles.tile(status(true, false), "owner", false)));
    }

    // An ownerless server that is down is Claim & Start: the claim makes the player its owner first.
    private static void ownerless() {
        for (String v : VERDICTS) {
            assertEq("ownerless, " + v, "CLAIM 认领并启动 false []", brief(MenuTiles.tile(status(false, true), v, true)));
        }
        assertEq("ownerless, English", "CLAIM Claim & Start false []",
                brief(MenuTiles.tile(status(false, true), "owner_only", false)));
    }

    // Down and owned: the verdict decides between Start and a grey tile with its reason.
    private static void stopped() {
        Map<String, String> zh = new HashMap<>();
        zh.put("owner", "WAKE 启动 true []");
        zh.put("wake", "WAKE 启动 false []");
        zh.put(null, "WAKE 启动 false []");
        zh.put("", "WAKE 启动 false []");
        zh.put("future_verdict", "WAKE 启动 false []");
        zh.put("owner_only", "LOCKED 无法启动 false [只有主人能启动这台服务器, 它运行时任何人都能加入]");
        zh.put("allowlist", "LOCKED 无法启动 false [你不在这台服务器的启动名单上, 它运行时任何人都能加入]");
        zh.put("retiring", "LOCKED 无法启动 false [这台服务器已被放弃或正在删除, 不能再启动]");
        zh.put("start_failed", "LOCKED 无法启动 false [多次启动失败，已停止自动重试, 需要在网页控制台处理]");
        for (String v : VERDICTS) {
            assertEq("stopped, " + v, zh.get(v), brief(MenuTiles.tile(status(false, false), v, true)));
        }

        Map<String, String> en = new HashMap<>();
        en.put("owner", "WAKE Start true []");
        en.put("owner_only", "LOCKED Can't start false [Only its owner can start it, Anyone can join while it's running]");
        en.put("allowlist", "LOCKED Can't start false [You're not on its start list, Anyone can join while it's running]");
        en.put("retiring", "LOCKED Can't start false [This server was given up or is being deleted, It can't be started]");
        en.put("start_failed", "LOCKED Can't start false [It failed to start several times, It needs a fix on the web console]");
        en.forEach((v, want) ->
                assertEq("stopped, English, " + v, want, brief(MenuTiles.tile(status(false, false), v, false))));
    }

    // Claim sends a ClaimRequest, Join and Start a WakeRequest, a grey tile nothing.
    private static void clicks() {
        ControlFrame down = status(false, false);
        assertEq("claim click", ControlFrame.claimRequest("Steve", "alpha"),
                MenuTiles.click(MenuTiles.tile(status(false, true), "wake", true), "Steve", "alpha"));
        assertEq("join click", ControlFrame.wakeRequest("Steve", "alpha"),
                MenuTiles.click(MenuTiles.tile(status(true, false), "owner_only", true), "Steve", "alpha"));
        assertEq("start click", ControlFrame.wakeRequest("Steve", "alpha"),
                MenuTiles.click(MenuTiles.tile(down, "owner", true), "Steve", "alpha"));
        assertEq("start click, no verdict", ControlFrame.wakeRequest("Steve", "alpha"),
                MenuTiles.click(MenuTiles.tile(down, null, true), "Steve", "alpha"));
        for (String v : List.of("owner_only", "allowlist", "retiring", "start_failed")) {
            assertEq("locked click sends nothing: " + v, null,
                    MenuTiles.click(MenuTiles.tile(down, v, true), "Steve", "alpha"));
        }
        assertEq("chat reason, zh", "只有主人能启动这台服务器，它运行时任何人都能加入。",
                MenuTiles.chatReason(MenuTiles.tile(down, "owner_only", true), true));
        assertEq("chat reason, en", "Only its owner can start it. Anyone can join while it's running.",
                MenuTiles.chatReason(MenuTiles.tile(down, "owner_only", false), false));
    }

    private static void phases() {
        String[][] table = {
                {"Running", "运行中", "Running"},
                {"Starting", "启动中", "Starting"},
                {"Stopping", "停止中", "Stopping"},
                {"Stopped", "已停止", "Stopped"},
                {"Failed", "启动失败", "Failed to start"},
                {"Unknown", "未知", "Unknown"},
                {null, "未知", "Unknown"},
                {"", "未知", "Unknown"},
                {"Hibernating", "未知", "Unknown"},
        };
        for (String[] row : table) {
            assertEq("phase " + row[0] + ", zh", row[1], MenuTiles.phase(row[0], true));
            assertEq("phase " + row[0] + ", en", row[2], MenuTiles.phase(row[0], false));
        }
    }

    // The verdicts ride the ListUpdate by position and must land on the right server,
    // on every page.
    private static void wire() {
        List<String> names = new ArrayList<>();
        List<String> access = new ArrayList<>();
        for (int i = 0; i < 50; i++) {
            names.add(String.format("s%02d", i));
            access.add(i == 0 ? "owner" : i == 47 ? "owner_only" : "");
        }
        ControlFrame list = Control.decode(Control.encode(ControlFrame.listUpdate(names, access)));
        Map<String, String> byName = MenuTiles.accessByName(list);
        assertEq("verdicts by name, unknowns left out", Map.of("s00", "owner", "s47", "owner_only"), byName);
        assertEq("names only: no verdicts",
                Map.of(), MenuTiles.accessByName(ControlFrame.listUpdate(List.of("a", "b"))));

        MenuHolder second = new MenuHolder(list.servers(), byName, 1);
        assertEq("page 2 starts at s45", "s45", second.servers().get(0));
        assertEq("page 2 knows s47's verdict", "owner_only", second.verdict("s47"));
        assertEq("... and s46 has none", null, second.verdict("s46"));
        assertEq("paging keeps every verdict", byName, second.access());
    }

    // status is a StatusUpdate for server "alpha" in the given state.
    private static ControlFrame status(boolean ready, boolean claimable) {
        return ControlFrame.statusUpdate("alpha", ready ? "Running" : "Stopped", ready, 0, 20, claimable);
    }

    private static String brief(MenuTiles.Tile t) {
        return t.kind() + " " + t.action() + " " + t.mine() + " " + t.reason();
    }

    private static void assertEq(String what, Object want, Object got) {
        if (want == null ? got != null : !want.equals(got)) {
            throw new AssertionError(what + ": got " + got + ", want " + want);
        }
        checks++;
    }
}
