package best.lolicon.felis.velocity;

import com.velocitypowered.api.command.CommandManager;
import com.velocitypowered.api.command.CommandMeta;
import com.velocitypowered.api.plugin.PluginContainer;
import com.velocitypowered.api.plugin.PluginDescription;
import com.velocitypowered.api.plugin.PluginManager;

import java.util.IdentityHashMap;
import java.util.Locale;
import java.util.Map;
import java.util.Optional;
import java.util.Set;
import java.util.TreeMap;

import static best.lolicon.felis.velocity.Fakes.UNANSWERED;
import static best.lolicon.felis.velocity.Fakes.fake;

/**
 * ServerCommandTest drives FelisVelocityPlugin.yieldServerCommand against a fake
 * command and plugin manager shaped like Velocity 3.5.1's: its built-ins are owned by
 * the virtual plugin whose id is "velocity", aliases are case-insensitive, and
 * unregistering removes only the alias named. Velocity's own /server goes, and so does
 * one with no owner (how older proxies registered built-ins); a /server another plugin
 * registered, or whose owner the plugin manager cannot name, stays; with no /server at
 * all nothing is touched. Framework free: a failed assertion throws.
 *
 * <p>Run: {@code ./gradlew routingTest} in plugins/velocity.
 */
public final class ServerCommandTest {

    private static int checks;

    public static void main(String[] args) {
        Object velocity = new Object();
        Object other = new Object();
        Plugins plugins = new Plugins();
        plugins.add(velocity, "velocity");
        plugins.add(other, "serverselector");

        Commands builtin = new Commands();
        builtin.add("server", velocity);
        builtin.add("glist", velocity);
        builtin.add("felis", other);
        assertEq("Velocity's /server is yielded", true,
                FelisVelocityPlugin.yieldServerCommand(builtin.manager, plugins.manager));
        assertEq("... and gone", Set.of("glist", "felis"), builtin.aliases());
        assertEq("... by one unregister", 1, builtin.unregisters);

        Commands ownerless = new Commands();
        ownerless.add("Server", null);
        assertEq("an ownerless /server is yielded", true,
                FelisVelocityPlugin.yieldServerCommand(ownerless.manager, plugins.manager));
        assertEq("... and gone", Set.of(), ownerless.aliases());

        Commands theirs = new Commands();
        theirs.add("server", other);
        assertEq("another plugin's /server is kept", false,
                FelisVelocityPlugin.yieldServerCommand(theirs.manager, plugins.manager));
        assertEq("... registered", Set.of("server"), theirs.aliases());
        assertEq("... untouched", 0, theirs.unregisters);

        Object lookalike = new Object();
        plugins.add(lookalike, "velocitytools");
        Commands near = new Commands();
        near.add("server", lookalike);
        assertEq("a plugin whose id only starts with velocity keeps its /server", false,
                FelisVelocityPlugin.yieldServerCommand(near.manager, plugins.manager));
        assertEq("... registered", Set.of("server"), near.aliases());

        Commands unknown = new Commands();
        unknown.add("server", new Object());
        assertEq("a /server of an owner no plugin claims is kept", false,
                FelisVelocityPlugin.yieldServerCommand(unknown.manager, plugins.manager));
        assertEq("... registered", Set.of("server"), unknown.aliases());

        Commands none = new Commands();
        none.add("glist", velocity);
        assertEq("no /server: nothing to yield", false,
                FelisVelocityPlugin.yieldServerCommand(none.manager, plugins.manager));
        assertEq("... nothing unregistered", 0, none.unregisters);
        assertEq("... the rest kept", Set.of("glist"), none.aliases());

        System.out.println("ServerCommandTest OK (" + checks + " checks)");
    }

    /** Commands holds one meta per alias, keyed the way Velocity keys them: lower case. */
    private static final class Commands {
        final Map<String, CommandMeta> byAlias = new TreeMap<>();
        int unregisters;
        final CommandManager manager = fake(CommandManager.class, (m, a) -> {
            switch (m) {
                case "getCommandMeta":
                    return byAlias.get(((String) a[0]).toLowerCase(Locale.ENGLISH));
                case "hasCommand":
                    return a.length == 1 && byAlias.containsKey(((String) a[0]).toLowerCase(Locale.ENGLISH));
                case "unregister":
                    if (a[0] instanceof String alias) {
                        unregisters++;
                        byAlias.remove(alias.toLowerCase(Locale.ENGLISH));
                        return null;
                    }
                    throw new UnsupportedOperationException("unregister(" + a[0].getClass().getSimpleName() + ")");
                default:
                    return UNANSWERED;
            }
        });

        void add(String alias, Object owner) {
            byAlias.put(alias.toLowerCase(Locale.ENGLISH), fake(CommandMeta.class, (m, a) -> {
                switch (m) {
                    case "getPlugin":
                        return owner;
                    case "getAliases":
                        return Set.of(alias.toLowerCase(Locale.ENGLISH));
                    default:
                        return UNANSWERED;
                }
            }));
        }

        Set<String> aliases() {
            return Set.copyOf(byAlias.keySet());
        }
    }

    /** Plugins maps each plugin instance to its container, as fromInstance does. */
    private static final class Plugins {
        final Map<Object, PluginContainer> byInstance = new IdentityHashMap<>();
        final PluginManager manager = fake(PluginManager.class, (m, a) ->
                "fromInstance".equals(m) ? fromInstance(a[0]) : UNANSWERED);

        // Velocity's own fromInstance refuses a null instance.
        private Optional<PluginContainer> fromInstance(Object instance) {
            if (instance == null) {
                throw new NullPointerException("instance");
            }
            return Optional.ofNullable(byInstance.get(instance));
        }

        void add(Object instance, String id) {
            PluginDescription description = fake(PluginDescription.class,
                    (m, a) -> "getId".equals(m) ? id : UNANSWERED);
            byInstance.put(instance, fake(PluginContainer.class, (m, a) -> {
                switch (m) {
                    case "getDescription":
                        return description;
                    case "getInstance":
                        return Optional.of(instance);
                    default:
                        return UNANSWERED;
                }
            }));
        }
    }

    private static void assertEq(String what, Object want, Object got) {
        if (want == null ? got != null : !want.equals(got)) {
            throw new AssertionError(what + ": got " + got + ", want " + want);
        }
        checks++;
    }
}
