package best.lolicon.felis.paper;

import best.lolicon.felis.link.ExperienceConfig;
import net.kyori.adventure.text.Component;
import net.kyori.adventure.text.event.ClickEvent;
import net.kyori.adventure.text.event.HoverEvent;
import net.kyori.adventure.text.format.NamedTextColor;
import net.kyori.adventure.text.format.TextDecoration;
import org.bukkit.Difficulty;
import org.bukkit.GameMode;
import org.bukkit.GameRules;
import org.bukkit.Material;
import org.bukkit.World;
import org.bukkit.block.Block;
import org.bukkit.entity.Entity;
import org.bukkit.entity.ItemFrame;
import org.bukkit.entity.Player;
import org.bukkit.entity.Projectile;
import org.bukkit.event.EventHandler;
import org.bukkit.event.EventPriority;
import org.bukkit.event.Listener;
import org.bukkit.event.block.Action;
import org.bukkit.event.block.BlockBreakEvent;
import org.bukkit.event.block.BlockBurnEvent;
import org.bukkit.event.block.BlockIgniteEvent;
import org.bukkit.event.block.BlockPlaceEvent;
import org.bukkit.event.entity.EntityDamageByEntityEvent;
import org.bukkit.event.entity.EntityDamageEvent;
import org.bukkit.event.entity.FoodLevelChangeEvent;
import org.bukkit.event.hanging.HangingBreakByEntityEvent;
import org.bukkit.event.player.PlayerArmorStandManipulateEvent;
import org.bukkit.event.player.PlayerBucketEmptyEvent;
import org.bukkit.event.player.PlayerBucketFillEvent;
import org.bukkit.event.player.PlayerInteractEntityEvent;
import org.bukkit.event.player.PlayerInteractEvent;
import org.bukkit.event.player.PlayerJoinEvent;
import org.bukkit.event.vehicle.VehicleDestroyEvent;
import org.bukkit.event.world.WorldLoadEvent;

import java.util.Map;
import java.util.logging.Level;
import java.util.logging.Logger;

/** Keeps the lobby a hub: peaceful worlds, no damage or hunger, and no block or entity changes without BUILD_PERMISSION. felis-experience.json overrides each default. */
final class LobbyGuard implements Listener {

    /** Lets a player build in the lobby (default: ops). */
    static final String BUILD_PERMISSION = "felis.lobby.build";

    private static final long NOON = 6000L;

    boolean guarded(Player player) {
        return settings.flag("protectBuild", true) && !player.hasPermission(BUILD_PERMISSION);
    }

    private final Logger log;
    private final ExperienceConfig settings;

    LobbyGuard(Logger log) {
        this(log, new ExperienceConfig(Map.of()));
    }

    LobbyGuard(Logger log, ExperienceConfig settings) {
        this.log = log;
        this.settings = settings;
    }

    /** Applies the world rules; a rule the server version refuses is logged and never disables the plugin. */
    void protect(World world) {
        try {
            applyRules(world);
        } catch (RuntimeException | LinkageError e) {
            log.log(Level.WARNING, "could not apply every lobby rule to world " + world.getName()
                    + "; the menu is unaffected", e);
        }
    }

    void applyRules(World world) {
        world.setDifficulty(Difficulty.valueOf(settings.text("difficulty", "PEACEFUL")));
        // The nether and the end have no time of day and refuse setTime.
        if (settings.flag("freezeTime", true) && !world.isFixedTime()) {
            world.setTime(Math.floorMod(settings.number("time", NOON), 24000L));
        }
        if (settings.flag("clearWeather", true)) {
            world.setStorm(false);
            world.setThundering(false);
        }
        world.setGameRule(GameRules.PVP, settings.flag("pvp", false));
        world.setGameRule(GameRules.SPAWN_MOBS, settings.flag("spawnMobs", false));
        world.setGameRule(GameRules.SPAWN_WANDERING_TRADERS, settings.flag("spawnMobs", false));
        world.setGameRule(GameRules.MOB_GRIEFING, settings.flag("mobGriefing", false));
        world.setGameRule(GameRules.TNT_EXPLODES, settings.flag("tntExplodes", false));
        world.setGameRule(GameRules.KEEP_INVENTORY, settings.flag("keepInventory", true));
        world.setGameRule(GameRules.IMMEDIATE_RESPAWN, settings.flag("immediateRespawn", true));
        world.setGameRule(GameRules.SHOW_ADVANCEMENT_MESSAGES, settings.flag("showAdvancementMessages", false));
        world.setGameRule(GameRules.ADVANCE_TIME, !settings.flag("freezeTime", true));
        world.setGameRule(GameRules.ADVANCE_WEATHER, !settings.flag("clearWeather", true));
    }

    @EventHandler
    public void onWorldLoad(WorldLoadEvent event) {
        protect(event.getWorld());
    }

    @EventHandler
    public void onJoin(PlayerJoinEvent event) {
        Player player = event.getPlayer();
        if (settings.flag("disableHunger", true)) {
            player.setFoodLevel(20);
        }
        if (!player.hasPermission(BUILD_PERMISSION)) {
            player.setGameMode(GameMode.valueOf(settings.text("gameMode", "ADVENTURE")));
            if (settings.flag("teleportOnJoin", true)) {
                player.teleport(player.getWorld().getSpawnLocation());
            }
        }
        if (settings.flag("showWelcome", true)) {
            player.sendMessage(hint(FelisPaperPlugin.zh(player)));
        }
        if (settings.flag("openMenuOnJoin", false)) {
            player.performCommand("menu");
        }
    }

    Component hint(boolean zh) {
        Component open = Component.text(zh ? "[打开服务器菜单]" : "[Open the server menu]",
                        NamedTextColor.GREEN, TextDecoration.BOLD)
                .clickEvent(ClickEvent.runCommand("/menu"))
                .hoverEvent(HoverEvent.showText(Component.text(zh ? "点击运行 /menu" : "Click to run /menu")));
        return Component.text(settings.text(zh ? "welcomeZh" : "welcomeEn",
                        zh ? "欢迎来到大厅。输入 /menu 或点击 " : "Welcome to the lobby. Type /menu or click "),
                        NamedTextColor.GOLD)
                .append(open)
                .append(Component.text(zh ? "，选一个服务器进入。" : " to pick a server to join.",
                        NamedTextColor.GOLD));
    }

    @EventHandler(priority = EventPriority.LOW)
    public void onDamage(EntityDamageEvent event) {
        if (!(event.getEntity() instanceof Player player)) {
            return;
        }
        if (settings.flag("invulnerable", true)) {
            event.setCancelled(true);
        }
        if (settings.flag("voidRescue", true) && event.getCause() == EntityDamageEvent.DamageCause.VOID) {
            player.teleport(player.getWorld().getSpawnLocation());
        }
    }

    @EventHandler(priority = EventPriority.LOW)
    public void onHunger(FoodLevelChangeEvent event) {
        if (settings.flag("disableHunger", true) && event.getEntity() instanceof Player) {
            event.setCancelled(true);
        }
    }

    @EventHandler(priority = EventPriority.LOW)
    public void onBreak(BlockBreakEvent event) {
        if (guarded(event.getPlayer())) {
            event.setCancelled(true);
        }
    }

    @EventHandler(priority = EventPriority.LOW)
    public void onPlace(BlockPlaceEvent event) {
        if (guarded(event.getPlayer())) {
            event.setCancelled(true);
        }
    }

    @EventHandler(priority = EventPriority.LOW)
    public void onPour(PlayerBucketEmptyEvent event) {
        if (guarded(event.getPlayer())) {
            event.setCancelled(true);
        }
    }

    @EventHandler(priority = EventPriority.LOW)
    public void onScoop(PlayerBucketFillEvent event) {
        if (guarded(event.getPlayer())) {
            event.setCancelled(true);
        }
    }

    /** Stepping on farmland or turtle eggs destroys them; pressure plates stay usable. */
    @EventHandler(priority = EventPriority.LOW)
    public void onTrample(PlayerInteractEvent event) {
        Block block = event.getClickedBlock();
        if (event.getAction() != Action.PHYSICAL || block == null || !guarded(event.getPlayer())) {
            return;
        }
        Material type = block.getType();
        if (type == Material.FARMLAND || type == Material.TURTLE_EGG) {
            event.setCancelled(true);
        }
    }

    /** Fire spreads with nobody behind it, so only a builder may start one. */
    @EventHandler(priority = EventPriority.LOW)
    public void onIgnite(BlockIgniteEvent event) {
        if (settings.flag("protectBuild", true) &&
                (!(event.getIgnitingEntity() instanceof Player player) || guarded(player))) {
            event.setCancelled(true);
        }
    }

    @EventHandler(priority = EventPriority.LOW)
    public void onBurn(BlockBurnEvent event) {
        if (settings.flag("protectBuild", true)) {
            event.setCancelled(true);
        }
    }

    @EventHandler(priority = EventPriority.LOW)
    public void onHit(EntityDamageByEntityEvent event) {
        if (guardedCulprit(event.getDamager())) {
            event.setCancelled(true);
        }
    }

    @EventHandler(priority = EventPriority.LOW)
    public void onHangingBreak(HangingBreakByEntityEvent event) {
        if (guardedCulprit(event.getRemover())) {
            event.setCancelled(true);
        }
    }

    @EventHandler(priority = EventPriority.LOW)
    public void onVehicleDestroy(VehicleDestroyEvent event) {
        if (guardedCulprit(event.getAttacker())) {
            event.setCancelled(true);
        }
    }

    /** Right-clicking an item frame takes or turns what it holds. */
    @EventHandler(priority = EventPriority.LOW)
    public void onFrame(PlayerInteractEntityEvent event) {
        if (event.getRightClicked() instanceof ItemFrame && guarded(event.getPlayer())) {
            event.setCancelled(true);
        }
    }

    @EventHandler(priority = EventPriority.LOW)
    public void onArmorStand(PlayerArmorStandManipulateEvent event) {
        if (guarded(event.getPlayer())) {
            event.setCancelled(true);
        }
    }

    /** True when the entity is a guarded player, or a projectile one of them shot. */
    private boolean guardedCulprit(Entity culprit) {
        if (culprit instanceof Projectile projectile && projectile.getShooter() instanceof Player shooter) {
            return guarded(shooter);
        }
        return culprit instanceof Player player && guarded(player);
    }
}
