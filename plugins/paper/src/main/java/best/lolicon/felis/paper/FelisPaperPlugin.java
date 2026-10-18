package best.lolicon.felis.paper;

import best.lolicon.felis.link.Control;
import best.lolicon.felis.link.ControlFrame;
import best.lolicon.felis.link.ExperienceConfig;


import net.kyori.adventure.text.Component;
import net.kyori.adventure.text.format.NamedTextColor;
import net.kyori.adventure.text.format.TextDecoration;
import org.bukkit.Bukkit;
import org.bukkit.Material;
import org.bukkit.command.Command;
import org.bukkit.command.CommandSender;
import org.bukkit.entity.Player;
import org.bukkit.inventory.Inventory;
import org.bukkit.inventory.InventoryHolder;
import org.bukkit.inventory.ItemStack;
import org.bukkit.inventory.meta.ItemMeta;
import org.bukkit.plugin.java.JavaPlugin;

import java.util.ArrayList;
import java.util.List;

/** Lobby {@code /menu} and {@code /server} chest GUI. It holds no felis-api token: it only exchanges ControlFrames on felis:control, and the proxy talks to felis-api. */
public final class FelisPaperPlugin extends JavaPlugin {

    private LobbyMenu menu;
    private ExperienceConfig experience;

    @Override
    public void onEnable() {
        saveDefaultConfig();
        experience = ExperienceConfig.loadOrDefaults(ExperienceConfig.PATH, getLogger()::warning);
        if (!getConfig().getStringList("servers").isEmpty()) {
            getLogger().info("config.yml 'servers' is no longer read: the menu lists what the proxy routes.");
        }

        LobbyMenu menu = new LobbyMenu(PaperScreen::new,
                (task, ticks) -> getServer().getScheduler().runTaskLater(this, task, ticks), getLogger());
        this.menu = menu;
        getServer().getMessenger().registerOutgoingPluginChannel(this, Control.CHANNEL);
        getServer().getMessenger().registerIncomingPluginChannel(this, Control.CHANNEL, menu);
        getServer().getPluginManager().registerEvents(menu, this);

        // Worlds loaded before this point are protected here, later ones on their WorldLoadEvent.
        LobbyGuard guard = new LobbyGuard(getLogger(), experience);
        getServer().getPluginManager().registerEvents(guard, this);
        getServer().getWorlds().forEach(guard::protect);

        getLogger().info("felis-paper enabled: felis:control open, server list from the proxy. "
                + "Pure UI face — no felis-api token.");
    }

    @Override
    public boolean onCommand(CommandSender sender, Command command, String label, String[] args) {
        if (!(sender instanceof Player)) {
            sender.sendMessage(Component.text("Only a player can open the server menu.", NamedTextColor.RED));
            return true;
        }
        menu.request((Player) sender);
        return true;
    }

    /** True when the client locale is zh-*, matching the Velocity rule. */
    static boolean zh(Player player) {
        return "zh".equalsIgnoreCase(player.locale().getLanguage());
    }

    private final class PaperScreen implements LobbyMenu.Screen {
        private final Player player;

        PaperScreen(Player player) {
            this.player = player;
        }

        @Override
        public String name() {
            return player.getName();
        }

        @Override
        public boolean zh() {
            return FelisPaperPlugin.zh(player);
        }

        @Override
        public String menuTitle() {
            return experience.text(zh() ? "menuTitleZh" : "menuTitleEn", zh() ? "Felis 服务器" : "Felis Servers");
        }

        @Override
        public boolean online() {
            return player.isOnline();
        }

        @Override
        public void send(ControlFrame frame) {
            player.sendPluginMessage(FelisPaperPlugin.this, Control.CHANNEL, Control.encode(frame));
        }

        @Override
        public void chat(String text, LobbyMenu.Tone tone) {
            player.sendMessage(Component.text(text, tone == LobbyMenu.Tone.ERROR ? NamedTextColor.RED : NamedTextColor.YELLOW));
        }

        @Override
        public void open(MenuHolder holder, String title, int size, LobbyMenu.Slot[] slots) {
            Inventory inv = Bukkit.createInventory(holder, size,
                    Component.text(title, NamedTextColor.AQUA).decoration(TextDecoration.ITALIC, false));
            holder.setInventory(inv);
            boolean zh = zh();
            for (int i = 0; i < slots.length; i++) {
                if (slots[i] != null) {
                    inv.setItem(i, item(slots[i], zh));
                }
            }
            player.openInventory(inv);
        }

        @Override
        public void paint(MenuHolder holder, int slot, LobbyMenu.Slot content) {
            holder.getInventory().setItem(slot, item(content, zh()));
        }

        @Override
        public MenuHolder shown() {
            InventoryHolder holder = player.getOpenInventory().getTopInventory().getHolder();
            return holder instanceof MenuHolder menuHolder ? menuHolder : null;
        }

        @Override
        public void close() {
            player.closeInventory();
        }
    }

    private static ItemStack item(LobbyMenu.Slot slot, boolean zh) {
        return switch (slot) {
            case LobbyMenu.Loading l -> loadingTile(l.server(), zh);
            case LobbyMenu.Status s -> tile(s.status(), s.tile(), zh);
            case LobbyMenu.Unavailable u -> unavailableTile(u.server(), u.reason());
            case LobbyMenu.Nav n -> navItem(n.arrow() ? Material.ARROW : Material.PAPER, n.label());
        };
    }

    private static ItemStack navItem(Material material, String label) {
        ItemStack item = new ItemStack(material);
        ItemMeta meta = item.getItemMeta();
        meta.displayName(Component.text(label, NamedTextColor.YELLOW).decoration(TextDecoration.ITALIC, false));
        item.setItemMeta(meta);
        return item;
    }

    private static ItemStack tile(ControlFrame f, MenuTiles.Tile t, boolean zh) {
        Material material;
        NamedTextColor color;
        switch (t.kind()) {
            case CLAIM -> {
                material = Material.GOLD_BLOCK;
                color = NamedTextColor.GOLD;
            }
            case JOIN -> {
                material = Material.LIME_CONCRETE;
                color = NamedTextColor.GREEN;
            }
            case LOCKED -> {
                material = Material.GRAY_CONCRETE;
                color = NamedTextColor.GRAY;
            }
            default -> {
                material = Material.RED_CONCRETE;
                color = NamedTextColor.RED;
            }
        }
        ItemStack item = new ItemStack(material);
        ItemMeta meta = item.getItemMeta();
        meta.displayName(Component.text((t.mine() ? "★ " : "") + t.action() + "  ·  " + f.server(), color)
                .decoration(TextDecoration.ITALIC, false));
        List<Component> lore = new ArrayList<>();
        if (t.mine()) {
            lore.add(Component.text(zh ? "你的服务器" : "Your server", NamedTextColor.GOLD)
                    .decoration(TextDecoration.ITALIC, false));
        }
        lore.add(line(zh ? "状态" : "Status", MenuTiles.phase(f.phase(), zh)));
        lore.add(line(zh ? "在线" : "Players", f.playersOnline() + "/" + f.playersMax()));
        for (String r : t.reason()) {
            lore.add(Component.text(r, NamedTextColor.YELLOW).decoration(TextDecoration.ITALIC, false));
        }
        meta.lore(lore);
        item.setItemMeta(meta);
        return item;
    }

    private static ItemStack loadingTile(String server, boolean zh) {
        ItemStack item = new ItemStack(Material.GRAY_STAINED_GLASS_PANE);
        ItemMeta meta = item.getItemMeta();
        meta.displayName(Component.text(server, NamedTextColor.GRAY).decoration(TextDecoration.ITALIC, false));
        meta.lore(List.of(Component.text(zh ? "加载中…" : "Loading…", NamedTextColor.DARK_GRAY)
                .decoration(TextDecoration.ITALIC, false)));
        item.setItemMeta(meta);
        return item;
    }

    private static ItemStack unavailableTile(String server, String reason) {
        ItemStack item = new ItemStack(Material.BARRIER);
        ItemMeta meta = item.getItemMeta();
        meta.displayName(Component.text(server, NamedTextColor.DARK_GRAY).decoration(TextDecoration.ITALIC, false));
        meta.lore(List.of(Component.text(reason, NamedTextColor.GRAY).decoration(TextDecoration.ITALIC, false)));
        item.setItemMeta(meta);
        return item;
    }

    private static Component line(String key, String value) {
        return Component.text(key + ": ", NamedTextColor.GRAY)
                .append(Component.text(value, NamedTextColor.WHITE))
                .decoration(TextDecoration.ITALIC, false);
    }
}
