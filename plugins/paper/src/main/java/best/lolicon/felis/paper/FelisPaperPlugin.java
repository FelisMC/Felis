package best.lolicon.felis.paper;

import best.lolicon.felis.link.Control;
import best.lolicon.felis.link.ControlFrame;

import net.kyori.adventure.text.Component;
import net.kyori.adventure.text.format.NamedTextColor;
import net.kyori.adventure.text.format.TextDecoration;
import org.bukkit.Bukkit;
import org.bukkit.Material;
import org.bukkit.command.Command;
import org.bukkit.command.CommandSender;
import org.bukkit.entity.Player;
import org.bukkit.event.EventHandler;
import org.bukkit.event.Listener;
import org.bukkit.event.inventory.InventoryClickEvent;
import org.bukkit.event.inventory.InventoryDragEvent;
import org.bukkit.inventory.Inventory;
import org.bukkit.inventory.ItemStack;
import org.bukkit.inventory.meta.ItemMeta;
import org.bukkit.plugin.java.JavaPlugin;
import org.bukkit.plugin.messaging.PluginMessageListener;

import java.util.ArrayList;
import java.util.List;

/**
 * FelisPaperPlugin is the felis-paper lobby face (spec §12): the {@code /menu} (and
 * {@code /server}) chest GUI players use to pick, wake or claim a backend without
 * ever touching the command line. It is the player-facing end of the §27 scenario-10
 * path — {@code /menu → plugin msg → velocity → api → 共用等待队列 → ready 后 Connect}.
 *
 * <p><b>Pure UI face.</b> This plugin deliberately holds no felis-api token, opens no
 * HTTP connection, and keeps no waiting queue. Every action it takes is a single
 * {@link ControlFrame} written to the {@code felis:control} plugin-message channel,
 * and every piece of state it shows arrives as a frame on the same channel. The
 * Velocity proxy ({@code ControlChannel}) is the only thing that talks to felis-api,
 * and it derives the acting player's identity from the backend connection rather than
 * anything this lobby sends (spec §14) — so even a fully compromised lobby cannot act
 * as another player or reach the API directly. The build enforces this physically:
 * only the channel codec ({@code Control}/{@code ControlFrame}/{@code Json}) is
 * compiled in from the shared core; {@code FelisApiClient} and the token config are
 * not on the lobby's classpath at all.
 *
 * <p><b>Flow.</b> Opening the menu paints a "loading" tile per configured server and
 * fires a {@code StatusQuery} for each; the proxy answers with {@code StatusUpdate}
 * frames that repaint each tile by phase + ownership. Clicking a tile sends a
 * {@code ClaimRequest} when it is claimable (ownerless + stopped → "Claim &amp;
 * Start") or a {@code WakeRequest} otherwise (the single frame behind both the "Join"
 * of a running owned server and the "Wake" of a stopped owned one), then closes the
 * menu. A claim refusal comes back as an {@code Error} frame and is shown to the
 * player here; wake-path refusals (policy gate, capacity) are chat messages the
 * proxy's waiting queue sends directly. Readiness arrives as {@code TransferReady}
 * just before the proxy Connects them.
 */
public final class FelisPaperPlugin extends JavaPlugin implements Listener, PluginMessageListener {

    private static final int MAX_TILES = 54; // a double chest, the GUI ceiling

    /** Server names to show as tiles, in display order; loaded from config. */
    private final List<String> servers = new ArrayList<>();

    @Override
    public void onEnable() {
        saveDefaultConfig();
        servers.clear();
        servers.addAll(getConfig().getStringList("servers"));

        // Open both ends of felis:control. Outgoing carries Wake/Claim/StatusQuery to
        // the proxy; incoming receives StatusUpdate/TransferReady/Error back.
        getServer().getMessenger().registerOutgoingPluginChannel(this, Control.CHANNEL);
        getServer().getMessenger().registerIncomingPluginChannel(this, Control.CHANNEL, this);
        getServer().getPluginManager().registerEvents(this, this);

        getLogger().info("felis-paper enabled: " + servers.size()
                + " server tile(s), felis:control open. Pure UI face — no felis-api token.");
    }

    // ---- commands: /menu and /server both open the GUI ----

    @Override
    public boolean onCommand(CommandSender sender, Command command, String label, String[] args) {
        if (!(sender instanceof Player)) {
            sender.sendMessage(Component.text("Only a player can open the server menu.", NamedTextColor.RED));
            return true;
        }
        openMenu((Player) sender);
        return true;
    }

    private void openMenu(Player player) {
        boolean zh = zh(player);
        if (servers.isEmpty()) {
            player.sendMessage(Component.text(
                    zh ? "还没有配置任何服务器——请管理员先配置 felis-paper。"
                       : "No servers are configured yet — ask an operator to set up felis-paper.",
                    NamedTextColor.YELLOW));
            return;
        }
        int shown = Math.min(servers.size(), MAX_TILES);
        List<String> view = new ArrayList<>(servers.subList(0, shown));
        MenuHolder holder = new MenuHolder(view);
        Inventory inv = Bukkit.createInventory(holder, invSize(shown), menuTitle(zh));
        holder.setInventory(inv);
        for (int i = 0; i < shown; i++) {
            inv.setItem(i, loadingTile(view.get(i), zh));
        }
        player.openInventory(inv);
        // Ask the proxy for live status of every tile; answers repaint them.
        for (String server : view) {
            sendUpstream(player, ControlFrame.statusQuery(server));
        }
    }

    // ---- click: a tile is a button, never an item to pick up ----

    @EventHandler
    public void onInventoryClick(InventoryClickEvent event) {
        Inventory top = event.getView().getTopInventory();
        if (!(top.getHolder() instanceof MenuHolder)) {
            return; // not our GUI
        }
        // Every slot in our GUI is a button: cancel unconditionally so nothing can be
        // taken out, even on clicks in empty slots or the player's own inventory.
        event.setCancelled(true);
        if (event.getClickedInventory() != top) {
            return; // click landed in the player's inventory, not a tile
        }
        if (!(event.getWhoClicked() instanceof Player)) {
            return;
        }
        Player player = (Player) event.getWhoClicked();
        MenuHolder holder = (MenuHolder) top.getHolder();
        int slot = event.getSlot();
        if (slot < 0 || slot >= holder.servers().size()) {
            return; // padding slot
        }
        String server = holder.servers().get(slot);
        ControlFrame state = holder.latest(server);
        if (state == null) {
            return; // still loading — no status yet, so we don't know which frame to send
        }
        // Claimable (ownerless + stopped) → Claim & Start; everything else → Wake
        // (which the proxy treats as Join when the owned server is already running).
        if (state.claimable()) {
            sendUpstream(player, ControlFrame.claimRequest(player.getName(), server));
        } else {
            sendUpstream(player, ControlFrame.wakeRequest(player.getName(), server));
        }
        player.closeInventory();
    }

    @EventHandler
    public void onInventoryDrag(InventoryDragEvent event) {
        // A drag can deposit into or sweep across our tiles without ever firing a
        // single InventoryClickEvent on them, so the click guard alone is not enough:
        // cancel any drag that touches our GUI so a tile can never be grabbed or smeared.
        if (event.getView().getTopInventory().getHolder() instanceof MenuHolder) {
            event.setCancelled(true);
        }
    }

    // ---- downstream: felis:control frames from the proxy ----

    @Override
    public void onPluginMessageReceived(String channel, Player player, byte[] message) {
        if (!Control.CHANNEL.equals(channel)) {
            return;
        }
        ControlFrame frame;
        try {
            frame = Control.decode(message);
        } catch (IllegalArgumentException e) {
            getLogger().fine("Dropping malformed felis:control frame: " + e.getMessage());
            return;
        }
        switch (frame.type()) {
            case ControlFrame.STATUS_UPDATE:
                applyStatus(player, frame);
                break;
            case ControlFrame.ERROR:
                // The proxy already sanitizes transport faults; this is the only place
                // a claim/quota/policy refusal becomes visible to the player.
                player.sendMessage(Component.text("⚠ " + errorText(frame, zh(player)), NamedTextColor.RED));
                break;
            case ControlFrame.TRANSFER_READY:
                // The proxy performs the actual Connect; just make sure a stale menu is
                // not left open over the join.
                closeIfMenu(player);
                break;
            default:
                // Upstream-only types (Wake/Claim/StatusQuery) are never expected back.
        }
    }

    private void applyStatus(Player player, ControlFrame frame) {
        Inventory top = player.getOpenInventory().getTopInventory();
        if (!(top.getHolder() instanceof MenuHolder)) {
            return; // the player closed the menu before the answer arrived
        }
        MenuHolder holder = (MenuHolder) top.getHolder();
        int slot = holder.servers().indexOf(frame.server());
        if (slot < 0) {
            return; // a server we are not showing
        }
        holder.put(frame.server(), frame);
        top.setItem(slot, tile(frame, zh(player)));
    }

    private void closeIfMenu(Player player) {
        if (player.getOpenInventory().getTopInventory().getHolder() instanceof MenuHolder) {
            player.closeInventory();
        }
    }

    // ---- rendering ----

    private static Component menuTitle(boolean zh) {
        return Component.text(zh ? "Felis 服务器" : "Felis Servers", NamedTextColor.AQUA)
                .decoration(TextDecoration.ITALIC, false);
    }

    private ItemStack tile(ControlFrame f, boolean zh) {
        Material material;
        String action;
        NamedTextColor color;
        if (f.claimable()) {
            material = Material.GOLD_BLOCK;
            action = zh ? "认领并启动" : "Claim & Start";
            color = NamedTextColor.GOLD;
        } else if (f.ready()) {
            material = Material.LIME_CONCRETE;
            action = zh ? "加入" : "Join";
            color = NamedTextColor.GREEN;
        } else {
            material = Material.RED_CONCRETE;
            action = zh ? "唤醒" : "Wake";
            color = NamedTextColor.RED;
        }
        ItemStack item = new ItemStack(material);
        ItemMeta meta = item.getItemMeta();
        meta.displayName(Component.text(action + "  ·  " + f.server(), color)
                .decoration(TextDecoration.ITALIC, false));
        List<Component> lore = new ArrayList<>();
        lore.add(line(zh ? "状态" : "Status", f.phase() == null || f.phase().isEmpty() ? "?" : f.phase()));
        lore.add(line(zh ? "在线" : "Players", f.playersOnline() + "/" + f.playersMax()));
        meta.lore(lore);
        item.setItemMeta(meta);
        return item;
    }

    private ItemStack loadingTile(String server, boolean zh) {
        ItemStack item = new ItemStack(Material.GRAY_STAINED_GLASS_PANE);
        ItemMeta meta = item.getItemMeta();
        meta.displayName(Component.text(server, NamedTextColor.GRAY).decoration(TextDecoration.ITALIC, false));
        meta.lore(List.of(Component.text(zh ? "加载中…" : "Loading…", NamedTextColor.DARK_GRAY)
                .decoration(TextDecoration.ITALIC, false)));
        item.setItemMeta(meta);
        return item;
    }

    private static Component line(String key, String value) {
        return Component.text(key + ": ", NamedTextColor.GRAY)
                .append(Component.text(value, NamedTextColor.WHITE))
                .decoration(TextDecoration.ITALIC, false);
    }

    private static String errorText(ControlFrame f, boolean zh) {
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
                default:
                    break;
            }
        }
        return f.message() != null && !f.message().isEmpty()
                ? f.message()
                : (code != null ? code : (zh ? "请求失败，请重试。" : "Request failed — please try again."));
    }

    // ---- helpers ----

    /** zh mirrors the Velocity rule: render Chinese when the client locale is zh-*. */
    private static boolean zh(Player player) {
        return "zh".equalsIgnoreCase(player.locale().getLanguage());
    }

    private void sendUpstream(Player player, ControlFrame frame) {
        player.sendPluginMessage(this, Control.CHANNEL, Control.encode(frame));
    }

    private static int invSize(int count) {
        int rows = Math.max(1, (count + 8) / 9);
        return Math.min(rows, 6) * 9;
    }
}
