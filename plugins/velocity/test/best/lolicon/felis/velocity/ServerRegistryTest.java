package best.lolicon.felis.velocity;

import best.lolicon.felis.link.ServerView;

import java.net.InetSocketAddress;
import java.util.List;
import java.util.Optional;

/**
 * ServerRegistryTest drives the real ServerRegistry against a fake ProxyServer: a
 * refresh registers every server that has a backend address, leaves one registered at
 * the same address alone, moves one whose address changed, drops one that vanished
 * from a successful fetch, and host routing resolves only {@code <subdomain>.<root>}.
 * Framework free: a failed assertion throws.
 *
 * <p>Run: {@code ./gradlew routingTest} in plugins/velocity (it needs the velocity-api
 * classes the plugin compiles against).
 */
public final class ServerRegistryTest {

    private static int checks;

    public static void main(String[] args) {
        Fakes.Net net = new Fakes.Net();
        Fakes.Log log = new Fakes.Log();
        // velocity.toml's dead login placeholder, which the first refresh must replace.
        net.add("login", "127.0.0.1", 1);
        ServerRegistry reg = new ServerRegistry(net.proxy, log.logger, "MC.Example.test");

        reg.refresh(List.of(
                view("login", "login", true, "10.43.0.1:25565"),
                view("lobby", "lobby", true, "10.43.0.2:25565"),
                view("alpha", "alpha", false, "10.43.0.3:25566"),
                view("fresh", "fresh", false, null),
                view("", "blank", true, "10.43.0.9:25565")));
        assertEq("first refresh registrations",
                List.of("-login", "+login 10.43.0.1:25565", "+lobby 10.43.0.2:25565", "+alpha 10.43.0.3:25566"),
                List.copyOf(net.registrations));
        assertEq("login placeholder replaced", "10.43.0.1:25565", net.address("login"));
        assertEq("a server with no address is known", true, reg.isManaged("fresh"));
        assertEq("... but not registered", null, net.address("fresh"));
        assertEq("a nameless entry is skipped", false, reg.isManaged(""));
        assertEq("managed count", 4, reg.all().size());
        assertEq("registration logged", 1, log.count("INFO", "registered backend alpha -> 10.43.0.3:25566"));

        net.registrations.clear();
        reg.refresh(List.of(
                view("login", "login", true, "10.43.0.1:25565"),
                view("lobby", "lobby", true, "10.43.0.2:25565"),
                view("alpha", "alpha", true, "10.43.0.7:25566"),
                view("fresh", "fresh", true, "10.43.0.8:25565")));
        assertEq("second refresh: only the moved and the new",
                List.of("-alpha", "+alpha 10.43.0.7:25566", "+fresh 10.43.0.8:25565"),
                List.copyOf(net.registrations));
        assertEq("view follows the fetch", true, reg.view("alpha").ready());

        assertEq("host routing", "alpha", name(reg.resolveByHost("alpha.mc.example.test")));
        assertEq("host routing ignores case", "alpha", name(reg.resolveByHost("ALPHA.Mc.Example.Test")));
        assertEq("the root itself routes nowhere", null, name(reg.resolveByHost("mc.example.test")));
        assertEq("a look-alike suffix routes nowhere", null, name(reg.resolveByHost("alphamc.example.test")));
        assertEq("another domain routes nowhere", null, name(reg.resolveByHost("alpha.mc.example.test.evil")));
        assertEq("an unknown subdomain routes nowhere", null, name(reg.resolveByHost("nope.mc.example.test")));
        assertEq("no host routes nowhere", null, name(reg.resolveByHost(null)));

        net.registrations.clear();
        reg.refresh(List.of(
                view("login", "login", true, "10.43.0.1:25565"),
                view("lobby", "lobby", true, "10.43.0.2:25565"),
                view("fresh", "fresh", true, "10.43.0.8:25565")));
        assertEq("a vanished server is dropped", List.of("-alpha"), List.copyOf(net.registrations));
        assertEq("... from the views", false, reg.isManaged("alpha"));
        assertEq("... and from host routing", null, name(reg.resolveByHost("alpha.mc.example.test")));
        assertEq("deregistration logged", 1, log.count("INFO", "deregistered backend alpha"));

        // A server whose subdomain changed answers on the new one only.
        reg.refresh(List.of(
                view("login", "login", true, "10.43.0.1:25565"),
                view("lobby", "lobby", true, "10.43.0.2:25565"),
                view("fresh", "renamed", true, "10.43.0.8:25565")));
        assertEq("new subdomain", "fresh", name(reg.resolveByHost("renamed.mc.example.test")));
        assertEq("old subdomain let go", null, name(reg.resolveByHost("fresh.mc.example.test")));

        // A subdomain handed to another server routes to the new holder.
        reg.refresh(List.of(
                view("login", "login", true, "10.43.0.1:25565"),
                view("lobby", "lobby", true, "10.43.0.2:25565"),
                view("other", "renamed", true, "10.43.0.5:25565"),
                view("fresh", "fresh", true, "10.43.0.8:25565")));
        assertEq("subdomain moved", "other", name(reg.resolveByHost("renamed.mc.example.test")));
        assertEq("subdomain taken back", "fresh", name(reg.resolveByHost("fresh.mc.example.test")));

        // An empty successful fetch really does empty the proxy.
        net.registrations.clear();
        reg.refresh(List.of());
        assertEq("everything deregistered", 4, net.registrations.size());
        assertEq("nothing managed", 0, reg.all().size());

        assertAddr("host:port", "10.43.0.1", 25570, ServerRegistry.parseAddress("10.43.0.1:25570"));
        assertAddr("bare host takes the Minecraft port", "backend.svc", 25565,
                ServerRegistry.parseAddress("backend.svc"));
        assertAddr("a non-numeric port is not a port", "backend:x", 25565,
                ServerRegistry.parseAddress("backend:x"));

        System.out.println("ServerRegistryTest OK (" + checks + " checks)");
    }

    private static ServerView view(String name, String sub, boolean ready, String addr) {
        return new ServerView(name, sub, ready ? "Running" : "Stopped", ready, "ownerOnly",
                "Running", "ClusterIP", addr, 0, 20);
    }

    private static String name(Optional<ServerView> v) {
        return v.map(ServerView::name).orElse(null);
    }

    private static void assertAddr(String what, String host, int port, InetSocketAddress got) {
        assertEq(what + " host", host, got.getHostString());
        assertEq(what + " port", port, got.getPort());
        assertEq(what + " stays unresolved", true, got.isUnresolved());
    }

    private static void assertEq(String what, Object want, Object got) {
        if (want == null ? got != null : !want.equals(got)) {
            throw new AssertionError(what + ": got " + got + ", want " + want);
        }
        checks++;
    }
}
