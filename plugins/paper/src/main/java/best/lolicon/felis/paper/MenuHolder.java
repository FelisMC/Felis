package best.lolicon.felis.paper;

import best.lolicon.felis.link.ControlFrame;

import org.bukkit.inventory.Inventory;
import org.bukkit.inventory.InventoryHolder;

import java.util.HashMap;
import java.util.List;
import java.util.Map;

/** State carried by a {@code /menu} inventory; its holder type tells this plugin's GUI from any other open chest. */
final class MenuHolder implements InventoryHolder {

    /** Tiles per page: five rows, leaving the bottom row for page navigation. */
    static final int PAGE_SIZE = 45;

    private final List<String> all;
    private final Map<String, String> access;                 // server → verdict, when known
    private final int page;
    private final List<String> servers;                       // this page; index = slot
    private final Map<String, ControlFrame> latest = new HashMap<>();
    private Inventory inventory;

    MenuHolder(List<String> all, Map<String, String> access, int page) {
        this.all = all;
        this.access = access;
        this.page = page;
        int from = Math.min(page * PAGE_SIZE, all.size());
        this.servers = all.subList(from, Math.min(from + PAGE_SIZE, all.size()));
    }

    static int pageCount(int total) {
        return Math.max(1, (total + PAGE_SIZE - 1) / PAGE_SIZE);
    }

    List<String> all() {
        return all;
    }

    Map<String, String> access() {
        return access;
    }

    /** Returns felis-api's verdict for one server, or null when none came. */
    String verdict(String server) {
        return access.get(server);
    }

    int page() {
        return page;
    }

    int pages() {
        return pageCount(all.size());
    }

    /** Returns this page's tile order; the list index is the inventory slot. */
    List<String> servers() {
        return servers;
    }

    /** Returns the most recent StatusUpdate for a server, or null if none yet. */
    ControlFrame latest(String server) {
        return latest.get(server);
    }

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
