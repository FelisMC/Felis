package best.lolicon.felis.paper;

import best.lolicon.felis.paper.Fakes.FakePlayer;
import best.lolicon.felis.paper.Fakes.FakeWorld;

import net.kyori.adventure.text.Component;
import net.kyori.adventure.text.event.ClickEvent;
import org.bukkit.GameMode;
import org.bukkit.Material;
import org.bukkit.block.BlockFace;
import org.bukkit.damage.DamageSource;
import org.bukkit.entity.ArmorStand;
import org.bukkit.entity.Arrow;
import org.bukkit.entity.Cow;
import org.bukkit.entity.Entity;
import org.bukkit.entity.ItemFrame;
import org.bukkit.entity.Minecart;
import org.bukkit.entity.Painting;
import org.bukkit.entity.Player;
import org.bukkit.entity.Villager;
import org.bukkit.event.Cancellable;
import org.bukkit.event.block.Action;
import org.bukkit.event.block.BlockBreakEvent;
import org.bukkit.event.block.BlockBurnEvent;
import org.bukkit.event.block.BlockIgniteEvent;
import org.bukkit.event.block.BlockPlaceEvent;
import org.bukkit.event.entity.EntityDamageByEntityEvent;
import org.bukkit.event.entity.EntityDamageEvent;
import org.bukkit.event.entity.EntityDamageEvent.DamageCause;
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
import org.bukkit.inventory.EquipmentSlot;

import java.util.ArrayList;
import java.util.List;
import java.util.Locale;
import java.util.function.Function;
import java.util.logging.Handler;
import java.util.logging.Level;
import java.util.logging.LogRecord;
import java.util.logging.Logger;

/**
 * LobbyGuardTest drives the real LobbyGuard handlers with real paper-api events built
 * around fake players, worlds and entities: what a passer-by may not do to the lobby,
 * that a builder may, that nobody is hurt or starved and a fall into the void lands at
 * spawn, and what a player is told and set to on joining. Framework free: a failed
 * assertion throws.
 *
 * <p>The game rules resolve through the server's registry, which a fake cannot stand in
 * for; they are checked on a real Paper server. Here, the registry failing to load is
 * what a rule the server refuses looks like, and the guard has to log it and carry on.
 *
 * <p>Run: {@code ./gradlew lobbyTest} in plugins/paper.
 */
// The paper-api event constructors are marked for removal (the server builds them);
// a test raising the events itself has no other way in.
@SuppressWarnings("removal")
public final class LobbyGuardTest {

    private static final List<LogRecord> WARNINGS = new ArrayList<>();
    private static final LobbyGuard GUARD = new LobbyGuard(capturingLogger());
    private static final DamageSource HIT = Fakes.bare(DamageSource.class);

    private static int checks;
    private static FakeWorld world;

    public static void main(String[] args) {
        world = new FakeWorld();
        passerByLeavesNoMark();
        builderMayBuild();
        mechanismsStillWork();
        fire();
        nobodyIsHurt();
        joining();
        worldRules();
        System.out.println("LobbyGuardTest OK (" + checks + " checks)");
    }

    /** Every way a player could change the world, as the event a guarded or builder player raises. */
    private static List<Case> marks() {
        List<Case> cases = new ArrayList<>();
        cases.add(new Case("break a block", p -> new BlockBreakEvent(Fakes.block(Material.STONE), p)));
        cases.add(new Case("place a block", p -> new BlockPlaceEvent(Fakes.block(Material.DIRT), null,
                Fakes.block(Material.STONE), null, p, true, EquipmentSlot.HAND)));
        cases.add(new Case("pour a bucket", p -> new PlayerBucketEmptyEvent(p, Fakes.block(Material.AIR),
                Fakes.block(Material.STONE), BlockFace.UP, Material.LAVA_BUCKET, null, EquipmentSlot.HAND)));
        cases.add(new Case("scoop a bucket", p -> new PlayerBucketFillEvent(p, Fakes.block(Material.WATER),
                Fakes.block(Material.STONE), BlockFace.UP, Material.BUCKET, null, EquipmentSlot.HAND)));
        cases.add(new Case("trample farmland", p -> interact(p, Action.PHYSICAL, Material.FARMLAND)));
        cases.add(new Case("trample turtle eggs", p -> interact(p, Action.PHYSICAL, Material.TURTLE_EGG)));
        cases.add(new Case("hit a cow", p -> hit(p, Fakes.bare(Cow.class))));
        cases.add(new Case("hit a villager", p -> hit(p, Fakes.bare(Villager.class))));
        cases.add(new Case("shoot an armor stand", p -> hit(arrowFrom(p), Fakes.bare(ArmorStand.class))));
        cases.add(new Case("pop an item frame", p -> hit(p, Fakes.bare(ItemFrame.class))));
        cases.add(new Case("break a painting", p -> new HangingBreakByEntityEvent(Fakes.bare(Painting.class), p, HIT)));
        cases.add(new Case("shoot a painting down",
                p -> new HangingBreakByEntityEvent(Fakes.bare(Painting.class), arrowFrom(p), HIT)));
        cases.add(new Case("break a minecart", p -> new VehicleDestroyEvent(Fakes.bare(Minecart.class), HIT, p)));
        cases.add(new Case("turn an item frame", p -> new PlayerInteractEntityEvent(p, Fakes.bare(ItemFrame.class))));
        cases.add(new Case("strip an armor stand", p -> new PlayerArmorStandManipulateEvent(p,
                Fakes.bare(ArmorStand.class), null, null, EquipmentSlot.HEAD, EquipmentSlot.HAND)));
        cases.add(new Case("light a fire", p -> new BlockIgniteEvent(Fakes.block(Material.GRASS_BLOCK),
                BlockIgniteEvent.IgniteCause.FLINT_AND_STEEL, p)));
        return cases;
    }

    private static void passerByLeavesNoMark() {
        Player p = new FakePlayer(false, world, Locale.US).player;
        for (Case c : marks()) {
            assertEq("a passer-by cannot " + c.what, true, c.fire(p));
        }
    }

    private static void builderMayBuild() {
        Player p = new FakePlayer(true, world, Locale.US).player;
        for (Case c : marks()) {
            assertEq("a builder can " + c.what, false, c.fire(p));
        }
    }

    private static void mechanismsStillWork() {
        Player p = new FakePlayer(false, world, Locale.US).player;
        assertEq("a pressure plate still works", false,
                dispatch(interact(p, Action.PHYSICAL, Material.STONE_PRESSURE_PLATE)));
        assertEq("a button still works", false,
                dispatch(interact(p, Action.RIGHT_CLICK_BLOCK, Material.STONE_BUTTON)));
        assertEq("a chest still opens", false, dispatch(interact(p, Action.RIGHT_CLICK_BLOCK, Material.CHEST)));
        assertEq("talking to a villager still works", false,
                dispatch(new PlayerInteractEntityEvent(p, Fakes.bare(Villager.class))));
    }

    private static void fire() {
        assertEq("fire spreading on its own is stopped", true, dispatch(new BlockIgniteEvent(
                Fakes.block(Material.OAK_PLANKS), BlockIgniteEvent.IgniteCause.SPREAD, (Entity) null)));
        assertEq("lightning starts no fire", true, dispatch(new BlockIgniteEvent(
                Fakes.block(Material.GRASS_BLOCK), BlockIgniteEvent.IgniteCause.LIGHTNING, (Entity) null)));
        assertEq("fire burns nothing away", true, dispatch(new BlockBurnEvent(Fakes.block(Material.OAK_LOG), null)));
    }

    private static void nobodyIsHurt() {
        FakePlayer builder = new FakePlayer(true, world, Locale.US);
        for (DamageCause cause : new DamageCause[]{DamageCause.FALL, DamageCause.LAVA, DamageCause.DROWNING,
                DamageCause.ENTITY_ATTACK, DamageCause.STARVATION}) {
            FakePlayer p = new FakePlayer(false, world, Locale.US);
            assertEq("a player takes no " + cause + " damage", true,
                    dispatch(new EntityDamageEvent(p.player, cause, HIT, 4)));
            assertEq("... and stays where they are", 0, p.teleports.size());
        }
        assertEq("a builder takes no damage either", true,
                dispatch(new EntityDamageEvent(builder.player, DamageCause.FALL, HIT, 4)));
        FakePlayer other = new FakePlayer(false, world, Locale.US);
        assertEq("a player cannot hurt another", true,
                dispatch(new EntityDamageByEntityEvent(other.player, builder.player, DamageCause.ENTITY_ATTACK, HIT, 4)));

        FakePlayer faller = new FakePlayer(true, world, Locale.US);
        assertEq("falling out of the world does no damage", true,
                dispatch(new EntityDamageEvent(faller.player, DamageCause.VOID, HIT, 4)));
        assertEq("... and lands the player at spawn", List.of(world.spawn), faller.teleports);

        Cow cow = Fakes.bare(Cow.class);
        assertEq("a mob is not made immortal", false, dispatch(new EntityDamageEvent(cow, DamageCause.FALL, HIT, 4)));
        assertEq("nobody goes hungry", true,
                dispatch(new FoodLevelChangeEvent(new FakePlayer(false, world, Locale.US).player, 2)));
    }

    private static void joining() {
        FakePlayer p = new FakePlayer(false, world, Locale.US);
        GUARD.onJoin(new PlayerJoinEvent(p.player, Component.text("joined")));
        assertEq("a passer-by joins in adventure mode", GameMode.ADVENTURE, p.gameMode);
        assertEq("... at spawn", List.of(world.spawn), p.teleports);
        assertEq("... fed", 20, p.food);
        assertEq("... and told about the menu once", 1, p.messages.size());
        Component hint = p.messages.get(0);
        assertEq("the hint names /menu", true, Fakes.text(hint).contains("Type /menu"));
        assertEq("the hint runs /menu when clicked", List.of("/menu"), commands(hint));

        FakePlayer builder = new FakePlayer(true, world, Locale.US);
        builder.gameMode = GameMode.CREATIVE;
        GUARD.onJoin(new PlayerJoinEvent(builder.player, Component.text("joined")));
        assertEq("a builder keeps their game mode", GameMode.CREATIVE, builder.gameMode);
        assertEq("... and their place", 0, builder.teleports.size());
        assertEq("... and is told about the menu too", List.of("/menu"), commands(builder.messages.get(0)));

        FakePlayer zh = new FakePlayer(false, world, Locale.SIMPLIFIED_CHINESE);
        GUARD.onJoin(new PlayerJoinEvent(zh.player, Component.text("joined")));
        assertEq("a Chinese client is told in Chinese", true, Fakes.text(zh.messages.get(0)).contains("输入 /menu"));
        assertEq("... with the same click", List.of("/menu"), commands(zh.messages.get(0)));
    }

    private static void worldRules() {
        FakeWorld overworld = new FakeWorld("world", false, false);
        WARNINGS.clear();
        GUARD.onWorldLoad(new WorldLoadEvent(overworld.world));
        assertEq("a loaded world is made peaceful, noon and clear",
                List.of("setDifficulty PEACEFUL", "time 6000", "setStorm false", "setThundering false"),
                overworld.calls);

        FakeWorld nether = new FakeWorld("world_nether", true, true);
        GUARD.protect(nether.world);
        assertEq("a world without a clock is not given a time",
                List.of("setDifficulty PEACEFUL", "setStorm false", "setThundering false"), nether.calls);

        // Paper 26 throws for setTime on a clockless world; had the check missed one, the
        // plugin, and with it /menu, would not come up.
        FakeWorld odd = new FakeWorld("odd", false, true);
        WARNINGS.clear();
        GUARD.protect(odd.world);
        assertEq("a rule the server refuses is logged", true,
                WARNINGS.stream().anyMatch(r -> r.getMessage().contains("world odd")));
        assertEq("... as a warning, not a crash", Level.WARNING, WARNINGS.get(0).getLevel());
    }

    // ---- helpers ----

    private record Case(String what, Function<Player, Cancellable> event) {
        boolean fire(Player p) {
            return dispatch(event.apply(p));
        }
    }

    private static PlayerInteractEvent interact(Player p, Action action, Material type) {
        return new PlayerInteractEvent(p, action, null, Fakes.block(type), BlockFace.UP, EquipmentSlot.HAND);
    }

    private static EntityDamageByEntityEvent hit(Entity damager, Entity victim) {
        return new EntityDamageByEntityEvent(damager, victim, DamageCause.ENTITY_ATTACK, HIT, 4);
    }

    private static Arrow arrowFrom(Player shooter) {
        return Fakes.fake(Arrow.class, (m, a) -> m.equals("getShooter") ? shooter : Fakes.UNANSWERED);
    }

    /**
     * dispatch hands the event to every LobbyGuard handler that takes its type, the way
     * Bukkit would, and reports whether it ended up cancelled.
     */
    private static boolean dispatch(Cancellable event) {
        if (event instanceof PlayerArmorStandManipulateEvent e) {
            GUARD.onArmorStand(e);
        } else if (event instanceof PlayerInteractEntityEvent e) {
            GUARD.onFrame(e);
        } else if (event instanceof PlayerInteractEvent e) {
            GUARD.onTrample(e);
        } else if (event instanceof BlockBreakEvent e) {
            GUARD.onBreak(e);
        } else if (event instanceof BlockPlaceEvent e) {
            GUARD.onPlace(e);
        } else if (event instanceof PlayerBucketEmptyEvent e) {
            GUARD.onPour(e);
        } else if (event instanceof PlayerBucketFillEvent e) {
            GUARD.onScoop(e);
        } else if (event instanceof BlockIgniteEvent e) {
            GUARD.onIgnite(e);
        } else if (event instanceof BlockBurnEvent e) {
            GUARD.onBurn(e);
        } else if (event instanceof HangingBreakByEntityEvent e) {
            GUARD.onHangingBreak(e);
        } else if (event instanceof VehicleDestroyEvent e) {
            GUARD.onVehicleDestroy(e);
        } else if (event instanceof FoodLevelChangeEvent e) {
            GUARD.onHunger(e);
        } else if (event instanceof EntityDamageByEntityEvent e) {
            // Bukkit hands a by-entity hit to the plain damage handlers too.
            GUARD.onHit(e);
            GUARD.onDamage(e);
        } else if (event instanceof EntityDamageEvent e) {
            GUARD.onDamage(e);
        } else {
            throw new AssertionError("no handler for " + event.getClass().getSimpleName());
        }
        return event.isCancelled();
    }

    /** commands lists the run-command clicks anywhere in a component. */
    private static List<String> commands(Component c) {
        List<String> out = new ArrayList<>();
        ClickEvent<?> click = c.clickEvent();
        if (click != null && click.action() == ClickEvent.Action.RUN_COMMAND
                && click.payload() instanceof ClickEvent.Payload.Text text) {
            out.add(text.value());
        }
        for (Component child : c.children()) {
            out.addAll(commands(child));
        }
        return out;
    }

    private static Logger capturingLogger() {
        Logger log = Logger.getLogger("LobbyGuardTest");
        log.setUseParentHandlers(false);
        log.addHandler(new Handler() {
            @Override
            public void publish(LogRecord record) {
                if (record.getLevel().intValue() >= Level.WARNING.intValue()) {
                    WARNINGS.add(record);
                }
            }

            @Override
            public void flush() {
            }

            @Override
            public void close() {
            }
        });
        return log;
    }

    private static void assertEq(String what, Object want, Object got) {
        if (want == null ? got != null : !want.equals(got)) {
            throw new AssertionError(what + ": got " + got + ", want " + want);
        }
        checks++;
    }
}
