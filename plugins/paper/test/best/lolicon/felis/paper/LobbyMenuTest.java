package best.lolicon.felis.paper;

import best.lolicon.felis.link.Control;
import best.lolicon.felis.link.ControlFrame;

import org.bukkit.entity.Player;
import org.bukkit.event.inventory.ClickType;
import org.bukkit.event.inventory.InventoryAction;
import org.bukkit.event.inventory.InventoryClickEvent;
import org.bukkit.event.inventory.InventoryDragEvent;
import org.bukkit.event.inventory.InventoryType;
import org.bukkit.inventory.Inventory;
import org.bukkit.inventory.InventoryHolder;
import org.bukkit.inventory.InventoryView;
import org.bukkit.inventory.ItemStack;

import java.util.ArrayList;
import java.util.Arrays;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.UUID;
import java.util.logging.Level;
import java.util.logging.Logger;

import static best.lolicon.felis.paper.Fakes.UNANSWERED;
import static best.lolicon.felis.paper.Fakes.fake;

/**
 * LobbyMenuTest drives the real {@link LobbyMenu} the way Paper does: felis:control
 * frames through its plugin-message listener, and real paper-api click and drag events
 * over fake inventory views, with a fake screen per player recording what the menu
 * showed, painted, sent and said. It checks the menu a player actually gets: /menu
 * opens only on a list it is still waiting for, each tile starts as loading and is
 * repainted by its own status, a click sends the one frame its tile stands for (a grey
 * tile sends nothing and says why), paging walks the whole list with the verdicts
 * intact, a refused status query marks its tile while other refusals reach chat in the
 * player's language, and nothing in the menu can be picked up or dragged.
 *
 * <p>Run: {@code ./gradlew lobbyTest} in plugins/paper (plugins/test.sh does).
 */
public final class LobbyMenuTest {

    private static final Logger QUIET = Logger.getLogger("LobbyMenuTest");
    private static int checks;

    public static void main(String[] args) {
        QUIET.setLevel(Level.OFF);
        listOpensAsLoadingTiles();
        statusRepaintsItsOwnTile();
        clicksSendWhatTheTileSays();
        nothingCanBeTakenOrDragged();
        listWaitTimesOut();
        eachRequestGetsItsOwnWait();
        emptyListSaysSo();
        pagingWalksTheWholeList();
        refusalsReachThePlayer();
        errorTextInEachLanguage();
        transferReadyClosesOnlyTheMenu();
        strayFramesAreDropped();
        System.out.println("LobbyMenuTest OK (" + checks + " checks)");
    }

    private static void listOpensAsLoadingTiles() {
        Rig rig = new Rig();
        FakeScreen s = rig.screen("Steve", true);
        rig.menu.request(s.player);
        assertEq("/menu asks the proxy for the list", List.of(ControlFrame.listRequest()), s.sent);
        assertEq("... and opens nothing yet", 0, s.opened.size());

        rig.frame(s, ControlFrame.listUpdate(List.of("mine", "open", "locked"), List.of("owner", "wake", "owner_only")));
        assertEq("one page opens", 1, s.opened.size());
        Opened page = s.opened.get(0);
        assertEq("title in the player's language", "Felis 服务器", page.title);
        assertEq("one row for three servers", 9, page.size);
        assertEq("each tile loading, in list order",
                Arrays.asList(new LobbyMenu.Loading("mine"), new LobbyMenu.Loading("open"), new LobbyMenu.Loading("locked"),
                        null, null, null, null, null, null),
                Arrays.asList(page.slots));
        assertEq("a status query per tile, in order",
                List.of(ControlFrame.listRequest(), ControlFrame.statusQuery("mine"), ControlFrame.statusQuery("open"),
                        ControlFrame.statusQuery("locked")),
                s.sent);
        assertEq("the page is what the player sees", page.holder, s.shown);

        FakeScreen other = rig.screen("Alex", false);
        rig.frame(other, ControlFrame.listUpdate(List.of("mine")));
        assertEq("a list nobody asked for opens nothing", 0, other.opened.size());
    }

    private static void statusRepaintsItsOwnTile() {
        Rig rig = new Rig();
        FakeScreen s = rig.openMenu("Steve", false, List.of("mine", "open", "locked", "free"),
                List.of("owner", "wake", "owner_only", "wake"));
        rig.frame(s, up("open"));
        rig.frame(s, stopped("locked"));
        rig.frame(s, stopped("mine"));
        rig.frame(s, ownerless("free"));
        assertEq("an up server: Join", MenuTiles.Kind.JOIN, kind(s, 1));
        assertEq("owner_only and stopped: grey", MenuTiles.Kind.LOCKED, kind(s, 2));
        assertEq("the player's own stopped server: Start", MenuTiles.Kind.WAKE, kind(s, 0));
        assertTrue("... marked as theirs", ((LobbyMenu.Status) s.slots[0]).tile().mine());
        assertEq("ownerless: Claim", MenuTiles.Kind.CLAIM, kind(s, 3));
        assertEq("the tile keeps the frame it was painted from", up("open"), ((LobbyMenu.Status) s.slots[1]).status());

        int paints = s.paints;
        rig.frame(s, up("elsewhere"));
        assertEq("a server not on the page repaints nothing", paints, s.paints);
        s.close();
        rig.frame(s, up("open"));
        assertEq("a closed menu repaints nothing", paints, s.paints);
    }

    private static void clicksSendWhatTheTileSays() {
        Rig rig = new Rig();
        List<String> names = List.of("open", "locked", "free", "pending");
        FakeScreen s = rig.openMenu("Steve", true, names, List.of("wake", "owner_only", "wake", "wake"));
        rig.frame(s, up("open"));
        rig.frame(s, stopped("locked"));
        rig.frame(s, ownerless("free"));
        int sent = s.sent.size();

        rig.click(s, 3);
        assertEq("a loading tile sends nothing", sent, s.sent.size());
        rig.click(s, 7);
        assertEq("an empty slot sends nothing", sent, s.sent.size());

        rig.click(s, 1);
        assertEq("a grey tile sends nothing", sent, s.sent.size());
        assertEq("... says why in chat", "NOTICE ⚠ locked: 只有主人能启动这台服务器，它运行时任何人都能加入。", last(s.chat));
        assertTrue("... and leaves the menu open", s.shown != null);

        rig.click(s, 0);
        assertEq("Join sends a wake for this player", ControlFrame.wakeRequest("Steve", "open"), last(s.sent));
        assertEq("... and closes the menu", null, s.shown);

        FakeScreen c = rig.openMenu("Alex", false, names, List.of());
        rig.frame(c, ownerless("free"));
        rig.click(c, 2);
        assertEq("Claim sends a claim", ControlFrame.claimRequest("Alex", "free"), last(c.sent));
        assertEq("... and closes the menu", null, c.shown);
    }

    private static void nothingCanBeTakenOrDragged() {
        Rig rig = new Rig();
        FakeScreen s = rig.openMenu("Steve", false, List.of("open"), List.of());
        rig.frame(s, up("open"));
        int sent = s.sent.size();

        assertTrue("a click on a tile is cancelled", rig.click(s, 0).isCancelled());
        s.shown = s.opened.get(0).holder; // the click closed it; look again
        assertTrue("a click on an empty slot is cancelled", rig.click(s, 5).isCancelled());
        // Raw slot 9 is the player's own first slot, which Paper also numbers 0: the same
        // index as the Join tile above it.
        InventoryClickEvent own = rig.click(s, 9);
        assertTrue("a click in the player's own inventory is cancelled too", own.isCancelled());
        assertEq("... and acts on no tile", sent + 1, s.sent.size());
        assertTrue("a drag over the menu is cancelled", rig.drag(s.shown, 9).isCancelled());

        InventoryHolder chest = fake(InventoryHolder.class, (m, a) -> UNANSWERED);
        assertTrue("a click in some other chest is left alone", !rig.clickIn(s, chest, 27, 0).isCancelled());
        assertTrue("a drag in some other chest is left alone", !rig.drag(chest, 27).isCancelled());
        assertEq("... and sends nothing", sent + 1, s.sent.size());
    }

    private static void listWaitTimesOut() {
        Rig rig = new Rig();
        FakeScreen s = rig.screen("Steve", false);
        rig.menu.request(s.player);
        rig.ticks.advance(LobbyMenu.LIST_TIMEOUT_TICKS - 1);
        assertEq("no message while the list may still come", List.of(), s.chat);
        rig.ticks.advance(1);
        assertEq("the player hears the list is down",
                List.of("NOTICE The server list isn't available right now — please try again shortly."), s.chat);
        rig.frame(s, ControlFrame.listUpdate(List.of("open")));
        assertEq("a list after that opens nothing", 0, s.opened.size());

        FakeScreen gone = rig.screen("Alex", true);
        rig.menu.request(gone.player);
        gone.online = false;
        rig.ticks.advance(LobbyMenu.LIST_TIMEOUT_TICKS);
        assertEq("a player who left hears nothing", List.of(), gone.chat);

        FakeScreen answered = rig.screen("Kai", true);
        rig.menu.request(answered.player);
        rig.frame(answered, ControlFrame.listUpdate(List.of("open")));
        rig.ticks.advance(LobbyMenu.LIST_TIMEOUT_TICKS);
        assertEq("an answered /menu never says the list is down", List.of(), answered.chat);
    }

    private static void eachRequestGetsItsOwnWait() {
        Rig rig = new Rig();
        FakeScreen s = rig.screen("Steve", false);
        rig.menu.request(s.player);
        rig.ticks.advance(50);
        rig.menu.request(s.player); // /menu again before the first gave up
        rig.ticks.advance(20);      // past the first request's deadline
        assertEq("the first timeout does not cut the second wait short", List.of(), s.chat);
        rig.frame(s, ControlFrame.listUpdate(List.of("open")));
        assertEq("... so its list still opens", 1, s.opened.size());
        rig.ticks.advance(LobbyMenu.LIST_TIMEOUT_TICKS);
        assertEq("... and no timeout follows", List.of(), s.chat);
    }

    private static void emptyListSaysSo() {
        Rig rig = new Rig();
        FakeScreen s = rig.screen("Steve", true);
        rig.menu.request(s.player);
        rig.frame(s, ControlFrame.listUpdate(List.of()));
        assertEq("no servers: no menu", 0, s.opened.size());
        assertEq("... and a pointer to the console", List.of("NOTICE 还没有可加入的服务器——在网页控制台创建一个吧。"), s.chat);
        assertEq("a chest just big enough: 1, 9, 10, 45 tiles", List.of(9, 9, 18, 45),
                List.of(LobbyMenu.invSize(1), LobbyMenu.invSize(9), LobbyMenu.invSize(10), LobbyMenu.invSize(45)));
    }

    private static void pagingWalksTheWholeList() {
        Rig rig = new Rig();
        List<String> names = new ArrayList<>();
        List<String> access = new ArrayList<>();
        for (int i = 0; i < 100; i++) {
            names.add(String.format("s%03d", i));
            access.add(i == 50 ? "allowlist" : "wake");
        }
        FakeScreen s = rig.openMenu("Steve", false, names, access);
        Opened p1 = s.opened.get(0);
        assertEq("a long list opens a full chest", 54, p1.size);
        assertEq("title counts the pages", "Felis Servers  (1/3)", p1.title);
        assertEq("45 tiles on the first page", new LobbyMenu.Loading("s044"), p1.slots[44]);
        assertEq("no previous arrow on the first page", null, p1.slots[LobbyMenu.PREV_SLOT]);
        assertEq("the page number", new LobbyMenu.Nav(false, "1 / 3"), p1.slots[LobbyMenu.PAGE_SLOT]);
        assertEq("a next arrow", new LobbyMenu.Nav(true, "Next page"), p1.slots[LobbyMenu.NEXT_SLOT]);
        assertEq("status asked for this page only", 1 + 45, s.sent.size());

        rig.click(s, LobbyMenu.PREV_SLOT);
        assertEq("the empty previous slot on page one does nothing", 1, s.opened.size());
        rig.click(s, LobbyMenu.NEXT_SLOT);
        Opened p2 = s.opened.get(1);
        assertEq("next opens page two", "Felis Servers  (2/3)", p2.title);
        assertEq("... starting where page one ended", new LobbyMenu.Loading("s045"), p2.slots[0]);
        assertEq("... with a previous arrow", new LobbyMenu.Nav(true, "Previous page"), p2.slots[LobbyMenu.PREV_SLOT]);
        assertEq("... and asks for its own tiles", ControlFrame.statusQuery("s089"), last(s.sent));
        rig.frame(s, stopped("s050"));
        assertEq("the list's verdicts survive the paging", MenuTiles.Kind.LOCKED, kind(s, 5));

        rig.click(s, LobbyMenu.NEXT_SLOT);
        Opened p3 = s.opened.get(2);
        assertEq("the last page holds the rest", new LobbyMenu.Loading("s099"), p3.slots[9]);
        assertEq("... and no more", null, p3.slots[10]);
        assertEq("no next arrow on the last page", null, p3.slots[LobbyMenu.NEXT_SLOT]);
        int sent = s.sent.size();
        rig.click(s, LobbyMenu.NEXT_SLOT);
        rig.click(s, 20);
        assertEq("next and padding on the last page do nothing", List.of(3, sent), List.of(s.opened.size(), s.sent.size()));
        rig.click(s, LobbyMenu.PREV_SLOT);
        assertEq("previous goes back", "Felis Servers  (2/3)", s.opened.get(3).title);
    }

    private static void refusalsReachThePlayer() {
        Rig rig = new Rig();
        FakeScreen s = rig.openMenu("Steve", true, List.of("gone", "open"), List.of());
        rig.frame(s, ControlFrame.error("not_found", "server not found", "gone"));
        assertEq("a refused status query marks its tile", new LobbyMenu.Unavailable("gone", "这台服务器已不存在。"), s.slots[0]);
        assertEq("... instead of chat", List.of(), s.chat);

        rig.frame(s, up("open"));
        rig.frame(s, ControlFrame.error("cooldown", "woken recently", "open"));
        assertEq("a refused click on a painted tile goes to chat", "ERROR ⚠ 这台服务器刚被唤醒过，请稍候再试。", last(s.chat));
        assertEq("... and keeps the tile", MenuTiles.Kind.JOIN, kind(s, 1));

        rig.frame(s, ControlFrame.error("quota_exceeded", "quota", null));
        assertEq("a refusal naming no server goes to chat", "ERROR ⚠ 你已达到服务器配额上限。", last(s.chat));
        rig.frame(s, ControlFrame.error("already_claimed", "claimed", "elsewhere"));
        assertEq("a refusal for a server not on the page goes to chat", "ERROR ⚠ 该服务器已被认领。", last(s.chat));
        s.close();
        rig.frame(s, ControlFrame.error("at_capacity", "full", "gone"));
        assertEq("with the menu closed it goes to chat", "ERROR ⚠ 集群当前已满，请稍后再试。", last(s.chat));
    }

    private static void errorTextInEachLanguage() {
        String[][] same = {{"not_found", "bad_name"}, {"not_found", "invalid_server_name"}, {"transport_error", "interrupted"}};
        for (String[] pair : same) {
            for (boolean zh : new boolean[]{true, false}) {
                assertEq(pair[1] + " reads as " + pair[0], text(pair[0], "x", zh), text(pair[1], "y", zh));
            }
        }
        String[] codes = {"not_linked", "quota_exceeded", "already_claimed", "at_capacity", "cooldown", "forbidden",
                "not_found", "busy", "transport_error"};
        Map<String, String> seen = new HashMap<>();
        for (String code : codes) {
            for (boolean zh : new boolean[]{true, false}) {
                String t = text(code, "felis-api's words", zh);
                assertTrue(code + " has its own line", seen.put(t, code) == null);
                assertTrue(code + " is in the player's language: " + t, zh == t.codePoints().anyMatch(c -> c >= 0x4E00));
                assertTrue(code + " hides felis-api's words", !t.contains("felis-api's words"));
            }
        }
        assertTrue("not_linked names /link", text("not_linked", null, true).contains("/link") && text("not_linked", null, false).contains("/link"));
        assertEq("an unknown code shows felis-api's message in English", "The world is frozen.", text("new_code", "The world is frozen.", false));
        assertEq("... and a generic line in Chinese", "请求失败，请重试。", text("new_code", "The world is frozen.", true));
        assertEq("no code, no message: generic", "Request failed — please try again.", text(null, "", false));
        assertEq("no code, a message: the message", "Server is retiring.", text(null, "Server is retiring.", false));
    }

    private static void transferReadyClosesOnlyTheMenu() {
        Rig rig = new Rig();
        FakeScreen s = rig.openMenu("Steve", false, List.of("open"), List.of());
        rig.frame(s, ControlFrame.transferReady("Steve", "open"));
        assertEq("the menu closes before the move", List.of(1, "none"), List.of(s.closes, s.shown == null ? "none" : "open"));
        rig.frame(s, ControlFrame.transferReady("Steve", "open"));
        assertEq("with no menu open, nothing else is closed", 1, s.closes);
    }

    private static void strayFramesAreDropped() {
        Rig rig = new Rig();
        FakeScreen s = rig.screen("Steve", false);
        rig.menu.request(s.player);
        rig.menu.onPluginMessageReceived(Control.CHANNEL, s.player, new byte[]{1, 2, 3});
        rig.menu.onPluginMessageReceived("minecraft:brand", s.player, Control.encode(ControlFrame.listUpdate(List.of("open"))));
        assertEq("a malformed frame or another channel opens nothing", 0, s.opened.size());
        rig.frame(s, ControlFrame.wakeRequest("Steve", "open"));
        assertEq("an upstream-only frame does nothing", List.of(0, 0), List.of(s.opened.size(), s.chat.size()));
        rig.frame(s, ControlFrame.listUpdate(List.of("open")));
        assertEq("... and the /menu still opens on its list", 1, s.opened.size());
    }

    // ---- rig ----

    private static final class Rig {
        final Ticks ticks = new Ticks();
        final Map<Player, FakeScreen> screens = new HashMap<>();
        final LobbyMenu menu = new LobbyMenu(screens::get, ticks, QUIET);

        FakeScreen screen(String name, boolean zh) {
            FakeScreen s = new FakeScreen(name, zh);
            screens.put(s.player, s);
            return s;
        }

        /** openMenu runs /menu for a new player and answers it with a list. */
        FakeScreen openMenu(String name, boolean zh, List<String> servers, List<String> access) {
            FakeScreen s = screen(name, zh);
            menu.request(s.player);
            frame(s, access.isEmpty() ? ControlFrame.listUpdate(servers) : ControlFrame.listUpdate(servers, access));
            return s;
        }

        void frame(FakeScreen s, ControlFrame f) {
            menu.onPluginMessageReceived(Control.CHANNEL, s.player, Control.encode(f));
        }

        /** click clicks a raw slot of the menu the player has open. */
        InventoryClickEvent click(FakeScreen s, int rawSlot) {
            return clickIn(s, s.shown, s.shown == null ? 9 : s.shownSize(), rawSlot);
        }

        InventoryClickEvent clickIn(FakeScreen s, InventoryHolder holder, int size, int rawSlot) {
            InventoryClickEvent e = new InventoryClickEvent(view(holder, size, s.player), InventoryType.SlotType.CONTAINER,
                    rawSlot, ClickType.LEFT, InventoryAction.PICKUP_ALL);
            menu.onInventoryClick(e);
            return e;
        }

        InventoryDragEvent drag(InventoryHolder holder, int size) {
            Map<Integer, ItemStack> slots = new HashMap<>();
            slots.put(0, null);
            slots.put(1, null);
            InventoryDragEvent e = new InventoryDragEvent(view(holder, size, null), null, new ItemStack() {
            }, false, slots);
            menu.onInventoryDrag(e);
            return e;
        }

        // view is a chest of size slots above a 36-slot player inventory, as Paper lays
        // out an open container: raw slots count the chest first.
        private static InventoryView view(InventoryHolder holder, int size, Player player) {
            Inventory top = fake(Inventory.class, (m, a) -> switch (m) {
                case "getHolder" -> holder;
                case "getSize" -> size;
                default -> UNANSWERED;
            });
            Inventory bottom = fake(Inventory.class, (m, a) -> m.equals("getSize") ? 36 : UNANSWERED);
            return fake(InventoryView.class, (m, a) -> switch (m) {
                case "getTopInventory" -> top;
                case "getBottomInventory" -> bottom;
                case "getPlayer" -> player;
                case "convertSlot" -> (int) a[0] < size ? a[0] : (int) a[0] - size;
                case "getInventory" -> (int) a[0] < 0 ? null : (int) a[0] < size ? top : bottom;
                default -> UNANSWERED;
            });
        }
    }

    /** Ticks runs the menu's delayed tasks on a virtual main thread. */
    private static final class Ticks implements LobbyMenu.Later {
        private final List<Object[]> tasks = new ArrayList<>();
        private long now;

        @Override
        public void run(Runnable task, long ticks) {
            tasks.add(new Object[]{now + ticks, task});
        }

        void advance(long ticks) {
            now += ticks;
            List<Object[]> due = new ArrayList<>();
            tasks.removeIf(t -> (long) t[0] <= now && due.add(t));
            due.sort((x, y) -> Long.compare((long) x[0], (long) y[0]));
            due.forEach(t -> ((Runnable) t[1]).run());
        }
    }

    private record Opened(MenuHolder holder, String title, int size, LobbyMenu.Slot[] slots) {
    }

    /** FakeScreen is one player's client: what the menu showed, painted, sent and said. */
    private static final class FakeScreen implements LobbyMenu.Screen {
        final String name;
        final boolean zh;
        final Player player;
        final List<ControlFrame> sent = new ArrayList<>();
        final List<String> chat = new ArrayList<>();
        final List<Opened> opened = new ArrayList<>();
        boolean online = true;
        MenuHolder shown;
        LobbyMenu.Slot[] slots;
        int paints;
        int closes;

        FakeScreen(String name, boolean zh) {
            this.name = name;
            this.zh = zh;
            UUID id = UUID.randomUUID();
            this.player = fake(Player.class, (m, a) -> switch (m) {
                case "getUniqueId" -> id;
                case "getName" -> name;
                default -> UNANSWERED;
            });
        }

        int shownSize() {
            for (Opened o : opened) {
                if (o.holder == shown) {
                    return o.size;
                }
            }
            throw new AssertionError("no open page");
        }

        @Override
        public String name() {
            return name;
        }

        @Override
        public boolean zh() {
            return zh;
        }

        @Override
        public boolean online() {
            return online;
        }

        @Override
        public void send(ControlFrame frame) {
            sent.add(frame);
        }

        @Override
        public void chat(String text, LobbyMenu.Tone tone) {
            chat.add(tone + " " + text);
        }

        @Override
        public void open(MenuHolder holder, String title, int size, LobbyMenu.Slot[] content) {
            if (content.length != size) {
                throw new AssertionError("slots for " + content.length + " in a chest of " + size);
            }
            opened.add(new Opened(holder, title, size, content.clone()));
            shown = holder;
            slots = content.clone();
        }

        @Override
        public void paint(MenuHolder holder, int slot, LobbyMenu.Slot content) {
            if (holder != shown) {
                throw new AssertionError("painted a page that is not showing");
            }
            slots[slot] = content;
            paints++;
        }

        @Override
        public MenuHolder shown() {
            return shown;
        }

        @Override
        public void close() {
            shown = null;
            closes++;
        }
    }

    // ---- helpers ----

    private static ControlFrame up(String server) {
        return ControlFrame.statusUpdate(server, "Running", true, 3, 20, false);
    }

    private static ControlFrame stopped(String server) {
        return ControlFrame.statusUpdate(server, "Stopped", false, 0, 20, false);
    }

    private static ControlFrame ownerless(String server) {
        return ControlFrame.statusUpdate(server, "Stopped", false, 0, 20, true);
    }

    private static MenuTiles.Kind kind(FakeScreen s, int slot) {
        if (!(s.slots[slot] instanceof LobbyMenu.Status st)) {
            throw new AssertionError("slot " + slot + " holds " + s.slots[slot] + ", want a painted tile");
        }
        return st.tile().kind();
    }

    private static String text(String code, String message, boolean zh) {
        return LobbyMenu.errorText(ControlFrame.error(code, message, null), zh);
    }

    private static <T> T last(List<T> list) {
        return list.isEmpty() ? null : list.get(list.size() - 1);
    }

    private static void assertTrue(String what, boolean ok) {
        if (!ok) {
            throw new AssertionError(what);
        }
        checks++;
    }

    private static void assertEq(String what, Object want, Object got) {
        if (want == null ? got != null : !want.equals(got)) {
            throw new AssertionError(what + ": got " + got + ", want " + want);
        }
        checks++;
    }
}
