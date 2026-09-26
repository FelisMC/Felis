package best.lolicon.felis.paper;

import net.kyori.adventure.text.Component;
import net.kyori.adventure.text.TextComponent;
import org.bukkit.GameMode;
import org.bukkit.Location;
import org.bukkit.Material;
import org.bukkit.World;
import org.bukkit.block.Block;
import org.bukkit.entity.Player;

import java.lang.reflect.InvocationHandler;
import java.lang.reflect.Proxy;
import java.util.ArrayList;
import java.util.List;
import java.util.Locale;

/**
 * Fakes are the paper-api interfaces LobbyGuardTest drives the real listener with,
 * built with {@link Proxy}. They answer what the guard reads (a permission, a
 * world, its spawn, a block's type, a shooter) and record what it does to a player.
 */
final class Fakes {
    private Fakes() {
    }

    /** Returned by an {@link Answers} for a method it does not answer. */
    static final Object UNANSWERED = new Object();

    interface Answers {
        Object answer(String method, Object[] args) throws Throwable;
    }

    /**
     * fake builds an instance of an interface. Methods the answers leave unanswered
     * return a zero value; the guard reads nothing it is not told.
     */
    static <T> T fake(Class<T> type, Answers answers) {
        InvocationHandler h = (proxy, m, args) -> {
            Object[] a = args == null ? new Object[0] : args;
            if (m.getDeclaringClass() == Object.class) {
                switch (m.getName()) {
                    case "equals":
                        return proxy == a[0];
                    case "hashCode":
                        return System.identityHashCode(proxy);
                    default:
                        return type.getSimpleName() + "@fake";
                }
            }
            Object r = answers.answer(m.getName(), a);
            return r != UNANSWERED ? r : zero(m.getReturnType());
        };
        return type.cast(Proxy.newProxyInstance(type.getClassLoader(), new Class<?>[]{type}, h));
    }

    /** bare is an entity of the given type that answers nothing. */
    static <T> T bare(Class<T> type) {
        return fake(type, (m, a) -> UNANSWERED);
    }

    private static Object zero(Class<?> t) {
        if (t == boolean.class) {
            return false;
        }
        if (t == int.class || t == short.class || t == byte.class) {
            return 0;
        }
        if (t == long.class) {
            return 0L;
        }
        if (t == double.class || t == float.class) {
            return 0.0;
        }
        return null;
    }

    /**
     * A world whose spawn is a fixed location, and what was set on it. A clockless world
     * (the nether, the end) says so through isFixedTime; refusesTime makes setTime throw
     * the way Paper does for one.
     */
    static final class FakeWorld {
        final World world;
        final Location spawn;
        final List<String> calls = new ArrayList<>();

        FakeWorld() {
            this("world", false, false);
        }

        FakeWorld(String name, boolean fixedTime, boolean refusesTime) {
            Location[] at = new Location[1];
            world = fake(World.class, (m, a) -> {
                switch (m) {
                    case "getSpawnLocation":
                        return at[0].clone();
                    case "getName":
                        return name;
                    case "isFixedTime":
                        return fixedTime;
                    case "setTime":
                        if (refusesTime) {
                            throw new IllegalArgumentException("Cannot set time in world without world clock");
                        }
                        calls.add("time " + a[0]);
                        return null;
                    case "setDifficulty":
                    case "setStorm":
                    case "setThundering":
                        calls.add(m + " " + a[0]);
                        return null;
                    default:
                        return UNANSWERED;
                }
            });
            at[0] = new Location(world, 8.5, 70, -3.5);
            spawn = at[0];
        }
    }

    /** A player who either holds the lobby build permission or not, and what was done to them. */
    static final class FakePlayer {
        final Player player;
        final List<Location> teleports = new ArrayList<>();
        final List<Component> messages = new ArrayList<>();
        GameMode gameMode = GameMode.SURVIVAL;
        int food = 3;

        FakePlayer(boolean builder, FakeWorld world, Locale locale) {
            player = fake(Player.class, (m, a) -> {
                switch (m) {
                    case "hasPermission":
                        return builder && LobbyGuard.BUILD_PERMISSION.equals(a[0]);
                    case "getWorld":
                        return world.world;
                    case "locale":
                        return locale;
                    case "teleport":
                        teleports.add((Location) a[0]);
                        return true;
                    case "setGameMode":
                        gameMode = (GameMode) a[0];
                        return null;
                    case "getGameMode":
                        return gameMode;
                    case "setFoodLevel":
                        food = (Integer) a[0];
                        return null;
                    case "sendMessage":
                        if (a.length > 0 && a[0] instanceof Component c) {
                            messages.add(c);
                            return null;
                        }
                        return UNANSWERED;
                    default:
                        return UNANSWERED;
                }
            });
        }
    }

    static Block block(Material type) {
        return fake(Block.class, (m, a) -> m.equals("getType") ? type : UNANSWERED);
    }

    /** text is the plain text of a component built with Component.text. */
    static String text(Component c) {
        StringBuilder sb = new StringBuilder();
        if (c instanceof TextComponent t) {
            sb.append(t.content());
        }
        for (Component child : c.children()) {
            sb.append(text(child));
        }
        return sb.toString();
    }
}
