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
 * <p>It holds two things: the ordered list of server names (the list index <em>is</em>
 * the slot, so a click slot maps straight to a server) and the latest
 * {@link ControlFrame} seen for each, so a click knows whether to send a Claim or a
 * Wake without re-querying.
 */
final class MenuHolder implements InventoryHolder {

    private final List<String> servers;                       // index = slot
    private final Map<String, ControlFrame> latest = new HashMap<>();
    private Inventory inventory;

    MenuHolder(List<String> servers) {
        this.servers = servers;
    }

    /** servers returns the tile order; the list index is the inventory slot. */
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
