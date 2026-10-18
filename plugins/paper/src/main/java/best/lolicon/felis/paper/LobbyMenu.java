package best.lolicon.felis.paper;

import best.lolicon.felis.link.Control;
import best.lolicon.felis.link.ControlFrame;

import org.bukkit.entity.Player;
import org.bukkit.event.EventHandler;
import org.bukkit.event.Listener;
import org.bukkit.event.inventory.InventoryClickEvent;
import org.bukkit.event.inventory.InventoryDragEvent;
import org.bukkit.inventory.Inventory;
import org.bukkit.plugin.messaging.PluginMessageListener;

import java.util.List;
import java.util.Map;
import java.util.UUID;
import java.util.concurrent.ConcurrentHashMap;
import java.util.function.Function;
import java.util.logging.Logger;

/** Lobby {@code /menu} chest GUI: decides each slot's content ({@link Slot}); {@link Screen} draws it. */
final class LobbyMenu implements Listener, PluginMessageListener {

    // Bottom-row navigation slots on a paged menu.
    static final int PREV_SLOT = 45;
    static final int PAGE_SLOT = 49;
    static final int NEXT_SLOT = 53;
    static final long LIST_TIMEOUT_TICKS = 60L; // how long /menu waits for the proxy's ListUpdate

    /** One player's client as the menu sees it. */
    interface Screen {
        String name();

        /** True when the client locale is zh-*. */
        boolean zh();

        default String menuTitle() {
            return zh() ? "Felis 服务器" : "Felis Servers";
        }

        boolean online();

        void send(ControlFrame frame);

        void chat(String text, Tone tone);

        /** Shows a new menu page; slots[i] is slot i's content, null for none. */
        void open(MenuHolder holder, String title, int size, Slot[] slots);

        void paint(MenuHolder holder, int slot, Slot content);

        /** Returns the menu page in front of the player, or null. */
        MenuHolder shown();

        void close();
    }

    /** Chat line colour: a notice (yellow) or a refusal (red). */
    enum Tone { NOTICE, ERROR }

    sealed interface Slot permits Loading, Status, Unavailable, Nav {
    }

    /** A tile whose status has not come back yet. */
    record Loading(String server) implements Slot {
    }

    record Status(ControlFrame status, MenuTiles.Tile tile) implements Slot {
    }

    /** A tile whose status query the proxy refused. */
    record Unavailable(String server, String reason) implements Slot {
    }

    /** A bottom-row page control: an arrow, or the page number. */
    record Nav(boolean arrow, String label) implements Slot {
    }

    /** Runs a task on the main thread after a number of ticks. */
    interface Later {
        void run(Runnable task, long ticks);
    }

    private final Function<Player, Screen> screens;
    private final Later later;
    private final Logger log;

    // Token of each player's latest /menu request, so an earlier request's timeout cannot cut a later wait short.
    private final Map<UUID, Object> pendingOpen = new ConcurrentHashMap<>();

    LobbyMenu(Function<Player, Screen> screens, Later later, Logger log) {
        this.screens = screens;
        this.later = later;
        this.log = log;
    }

    /** Handles /menu: asks the proxy for the list and gives up after LIST_TIMEOUT_TICKS. */
    void request(Player player) {
        UUID id = player.getUniqueId();
        Screen screen = screens.apply(player);
        Object token = new Object();
        pendingOpen.put(id, token);
        screen.send(ControlFrame.listRequest());
        later.run(() -> {
            if (pendingOpen.remove(id, token) && screen.online()) {
                screen.chat(screen.zh() ? "暂时拿不到服务器列表，请稍后再试。"
                        : "The server list isn't available right now — please try again shortly.", Tone.NOTICE);
            }
        }, LIST_TIMEOUT_TICKS);
    }

    @Override
    public void onPluginMessageReceived(String channel, Player player, byte[] message) {
        if (!Control.CHANNEL.equals(channel)) {
            return;
        }
        ControlFrame frame;
        try {
            frame = Control.decode(message);
        } catch (IllegalArgumentException e) {
            log.fine("Dropping malformed felis:control frame: " + e.getMessage());
            return;
        }
        Screen screen = screens.apply(player);
        switch (frame.type()) {
            case ControlFrame.LIST_UPDATE:
                // A late answer after the timeout is dropped rather than opening an unasked menu.
                if (pendingOpen.remove(player.getUniqueId()) != null) {
                    openPage(screen, frame.servers(), MenuTiles.accessByName(frame), 0);
                }
                break;
            case ControlFrame.STATUS_UPDATE:
                applyStatus(screen, frame);
                break;
            case ControlFrame.ERROR:
                if (frame.server() != null && markUnavailable(screen, frame)) {
                    break; // a tile's status query failed: shown on the tile itself
                }
                screen.chat("⚠ " + errorText(frame, screen.zh()), Tone.ERROR);
                break;
            case ControlFrame.TRANSFER_READY:
                if (screen.shown() != null) {
                    screen.close();
                }
                break;
            default:
                // Upstream-only types (Wake/Claim/StatusQuery) are never expected back.
        }
    }

    private void openPage(Screen screen, List<String> all, Map<String, String> access, int page) {
        boolean zh = screen.zh();
        if (all.isEmpty()) {
            screen.chat(zh ? "还没有可加入的服务器——在网页控制台创建一个吧。"
                    : "There are no servers to join yet — create one on the web console.", Tone.NOTICE);
            return;
        }
        int pages = MenuHolder.pageCount(all.size());
        int p = Math.max(0, Math.min(page, pages - 1));
        MenuHolder holder = new MenuHolder(all, access, p);
        List<String> view = holder.servers();
        int size = pages > 1 ? 54 : invSize(view.size());
        Slot[] slots = new Slot[size];
        for (int i = 0; i < view.size(); i++) {
            slots[i] = new Loading(view.get(i));
        }
        if (pages > 1) {
            if (p > 0) {
                slots[PREV_SLOT] = new Nav(true, zh ? "上一页" : "Previous page");
            }
            slots[PAGE_SLOT] = new Nav(false, (p + 1) + " / " + pages);
            if (p < pages - 1) {
                slots[NEXT_SLOT] = new Nav(true, zh ? "下一页" : "Next page");
            }
        }
        screen.open(holder, title(screen.menuTitle(), p, pages), size, slots);
        for (String server : view) {
            screen.send(ControlFrame.statusQuery(server));
        }
    }

    private void applyStatus(Screen screen, ControlFrame frame) {
        MenuHolder holder = screen.shown();
        if (holder == null) {
            return; // the player closed the menu before the answer arrived
        }
        int slot = holder.servers().indexOf(frame.server());
        if (slot < 0) {
            return; // a server this menu does not show
        }
        holder.put(frame.server(), frame);
        screen.paint(holder, slot, new Status(frame, MenuTiles.tile(frame, holder.verdict(frame.server()), screen.zh())));
    }

    // Repaints a still-loading tile whose status query was refused; returns false when the error belongs to a click.
    private boolean markUnavailable(Screen screen, ControlFrame error) {
        MenuHolder holder = screen.shown();
        if (holder == null) {
            return false;
        }
        int slot = holder.servers().indexOf(error.server());
        if (slot < 0 || holder.latest(error.server()) != null) {
            return false;
        }
        screen.paint(holder, slot, new Unavailable(error.server(), errorText(error, screen.zh())));
        return true;
    }

    @EventHandler
    public void onInventoryClick(InventoryClickEvent event) {
        Inventory top = event.getView().getTopInventory();
        if (!(top.getHolder() instanceof MenuHolder holder)) {
            return; // not a GUI this plugin opened
        }
        // Cancel every click so no item can be taken out, including in empty slots and the player's inventory.
        event.setCancelled(true);
        if (event.getClickedInventory() != top) {
            return; // click landed in the player's inventory, not a tile
        }
        if (event.getWhoClicked() instanceof Player player) {
            click(screens.apply(player), holder, event.getSlot());
        }
    }

    @EventHandler
    public void onInventoryDrag(InventoryDragEvent event) {
        // A drag can fire no InventoryClickEvent on the tiles, so the click guard alone is not enough.
        if (event.getView().getTopInventory().getHolder() instanceof MenuHolder) {
            event.setCancelled(true);
        }
    }

    private void click(Screen screen, MenuHolder holder, int slot) {
        if (holder.pages() > 1 && (slot == PREV_SLOT || slot == NEXT_SLOT)) {
            int target = holder.page() + (slot == PREV_SLOT ? -1 : 1);
            if (target >= 0 && target < holder.pages()) {
                openPage(screen, holder.all(), holder.access(), target);
            }
            return;
        }
        if (slot < 0 || slot >= holder.servers().size()) {
            return; // padding slot
        }
        String server = holder.servers().get(slot);
        ControlFrame state = holder.latest(server);
        if (state == null) {
            return; // still loading: no status yet, so the frame to send is unknown
        }
        boolean zh = screen.zh();
        MenuTiles.Tile tile = MenuTiles.tile(state, holder.verdict(server), zh);
        ControlFrame click = MenuTiles.click(tile, screen.name(), server);
        if (click == null) {
            // Refused start: say why and keep the menu open.
            screen.chat("⚠ " + server + ": " + MenuTiles.chatReason(tile, zh), Tone.NOTICE);
            return;
        }
        screen.send(click);
        screen.close();
    }

    static String title(boolean zh, int page, int pages) {
        return title(zh ? "Felis 服务器" : "Felis Servers", page, pages);
    }

    static String title(String title, int page, int pages) {
        if (pages > 1) {
            title += "  (" + (page + 1) + "/" + pages + ")";
        }
        return title;
    }

    static String errorText(ControlFrame f, boolean zh) {
        String code = f.code();
        if (code != null) {
            switch (code) {
                case "not_linked":
                    return zh ? "请先绑定账号——运行 /link，然后在网页控制台完成绑定。"
                              : "Link your account first — run /link, then finish on the web console.";
                case "quota_exceeded":
                    return zh ? "你已达到服务器配额上限。"
                              : "You've reached your server quota.";
                case "already_claimed":
                    return zh ? "该服务器已被认领。"
                              : "That server is already claimed.";
                case "at_capacity":
                    return zh ? "集群当前已满，请稍后再试。"
                              : "The cluster is full right now — please try again later.";
                case "cooldown":
                    return zh ? "这台服务器刚被唤醒过，请稍候再试。"
                              : "That server was just woken — try again in a moment.";
                case "forbidden":
                    return zh ? "你没有权限这样做。"
                              : "You're not allowed to do that.";
                case "not_found":
                case "bad_name":
                case "invalid_server_name":
                    return zh ? "这台服务器已不存在。"
                              : "That server no longer exists.";
                case "busy":
                    return zh ? "Felis 现在很忙，请过一会儿再试。"
                              : "Felis is busy right now — try again in a moment.";
                case "transport_error":
                case "interrupted":
                    return zh ? "Felis 暂时不可用，请稍后再试。"
                              : "Felis is temporarily unavailable — please try again.";
                default:
                    break;
            }
        }
        // An unmapped code carries felis-api's English message; zh clients get a generic line.
        if (!zh && f.message() != null && !f.message().isEmpty()) {
            return f.message();
        }
        return zh ? "请求失败，请重试。" : "Request failed — please try again.";
    }

    /** Returns the smallest chest size (1-6 rows) that holds count tiles. */
    static int invSize(int count) {
        int rows = Math.max(1, (count + 8) / 9);
        return Math.min(rows, 6) * 9;
    }
}
