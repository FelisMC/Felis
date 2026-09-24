package best.lolicon.felis.paper;

import best.lolicon.felis.link.ControlFrame;

import org.bukkit.inventory.Inventory;
import org.bukkit.inventory.InventoryHolder;

import java.util.HashMap;
import java.util.List;
import java.util.Map;

/**
 * MenuHolder is the identity and state carried by a {@code /menu} inventory. Bukkit
 * lets an {@link InventoryHolder} ride along with an {@link Inventory}, which is how
 * {@link FelisPaperPlugin} tells "this is our GUI" from any other open chest — a click
 * or a downstream frame only acts when {@code inventory.getHolder() instanceof
 * MenuHolder}. Tying the state to the inventory instance (rather than a per-player map
 * on the plugin) means it is garbage-collected with the menu and never leaks across
 * reopen.
 *
 * <p>It holds the full server list the proxy sent (so paging needs no new request),
 * which page is showing, and the latest {@link ControlFrame} seen for each server on
 * it, so a click knows whether to send a Claim or a Wake without re-querying. Within
 * a page the list index <em>is</em> the slot.
 */
final class MenuHolder implements InventoryHolder {

    /** Tiles per page: five rows, leaving the bottom row for page navigation. */
    static final int PAGE_SIZE = 45;

    private final List<String> all;
    private final int page;
    private final List<String> servers;                       // this page; index = slot
    private final Map<String, ControlFrame> latest = new HashMap<>();
    private Inventory inventory;

    MenuHolder(List<String> all, int page) {
        this.all = all;
        this.page = page;
        int from = Math.min(page * PAGE_SIZE, all.size());
        this.servers = all.subList(from, Math.min(from + PAGE_SIZE, all.size()));
    }

    static int pageCount(int total) {
        return Math.max(1, (total + PAGE_SIZE - 1) / PAGE_SIZE);
    }

    /** all is the whole list the proxy sent, across every page. */
    List<String> all() {
        return all;
    }

    int page() {
        return page;
    }

    int pages() {
        return pageCount(all.size());
    }

    /** servers returns this page's tile order; the list index is the inventory slot. */
    List<String> servers() {
        return servers;
    }

    /** latest is the most recent StatusUpdate for a server, or null if none yet. */
    ControlFrame latest(String server) {
        return latest.get(server);
    }

    /** put records the latest StatusUpdate for a server. */
    void put(String server, ControlFrame frame) {
        latest.put(server, frame);
    }

    void setInventory(Inventory inventory) {
        this.inventory = inventory;
    }

    @Override
    public Inventory getInventory() {
        return inventory;
    }
}
