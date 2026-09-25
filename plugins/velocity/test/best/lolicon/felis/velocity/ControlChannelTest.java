package best.lolicon.felis.velocity;

import best.lolicon.felis.link.Control;
import best.lolicon.felis.link.ControlFrame;
import best.lolicon.felis.link.FelisApiClient;
import best.lolicon.felis.link.LinkConfig;
import best.lolicon.felis.link.ServerView;

import com.velocitypowered.api.event.connection.DisconnectEvent;
import com.velocitypowered.api.event.connection.PluginMessageEvent;
import com.velocitypowered.api.proxy.ServerConnection;
import com.velocitypowered.api.proxy.messages.ChannelIdentifier;
import com.velocitypowered.api.proxy.messages.MinecraftChannelIdentifier;
import com.velocitypowered.api.proxy.server.RegisteredServer;

import java.net.InetAddress;
import java.net.ServerSocket;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.util.ArrayList;
import java.util.List;

/**
 * ControlChannelTest drives the real ControlChannel (with the real WaitingRouter,
 * ServerRegistry, FelisApiClient and call pool behind it) through Velocity's
 * PluginMessageEvent, against a fake proxy and a stub felis-api: which frames are
 * consumed, which sources may act and as whom, what each lobby frame answers,
 * that status answers are shared across players, that a claim refusal never wakes,
 * and that the per-player frame budget holds. Framework free: a failed assertion
 * throws.
 *
 * <p>Run: {@code ./gradlew routingTest} in plugins/velocity.
 */
public final class ControlChannelTest {

    private static final String SERVERS = "/api/v1/internal/servers/";

    private static int checks;
    private static int nextPlayer = 1;

    private static Fakes.Net net;
    private static Fakes.Log log;
    private static Fakes.Api api;
    private static WaitingRouter router;
    private static ControlChannel channel;
    private static RegisteredServer login;
    private static RegisteredServer lobby;
    private static RegisteredServer beta;

    public static void main(String[] args) throws Exception {
        net = new Fakes.Net();
        log = new Fakes.Log();
        api = new Fakes.Api();
        try {
            FelisApiClient client = new FelisApiClient(new LinkConfig(api.url(), "t"));
            ServerRegistry reg = new ServerRegistry(net.proxy, log.logger, "mc.test");
            // Deliberately unsorted: the tile list must come back sorted.
            reg.refresh(List.of(
                    view("gamma", false, "10.43.0.5:25565"),
                    view("login", true, "10.43.0.1:25565"),
                    view("beta", true, "10.43.0.4:25565"),
                    view("lobby", true, "10.43.0.2:25565"),
                    view("alpha", false, "10.43.0.3:25565")));
            FelisVelocityPlugin plugin =
                    new FelisVelocityPlugin(net.proxy, log.logger, Files.createTempDirectory("felis-control"));
            router = new WaitingRouter(net.proxy, log.logger, client, reg, plugin, "login", "lobby");
            channel = new ControlChannel(net.proxy, log.logger, client, router, reg, plugin, "login", "lobby");
            login = net.proxy.getServer("login").orElseThrow();
            lobby = net.proxy.getServer("lobby").orElseThrow();
            beta = net.proxy.getServer("beta").orElseThrow();

            channel.register();
            assertEq("register opens felis:control", List.of("felis:control"), List.copyOf(net.channels));

            consumption();
            sources();
            statusQueries(reg, plugin);
            claims();
            wakeAndTransfer();
            budget();
        } finally {
            api.close();
        }
        System.out.println("ControlChannelTest OK (" + checks + " checks)");
    }

    private static void consumption() {
        Fakes.FakePlayer p = player(true);
        PluginMessageEvent other = message(p.on(lobby), MinecraftChannelIdentifier.create("other", "x"),
                Control.encode(ControlFrame.statusQuery("alpha")));
        assertEq("another channel is left alone", true, other.getResult().isAllowed());

        PluginMessageEvent fromClient = new PluginMessageEvent(p.player, p.on(lobby), ControlChannel.CHANNEL,
                Control.encode(ControlFrame.statusQuery("alpha")));
        channel.onPluginMessage(fromClient);
        assertEq("a client's frame is consumed", false, fromClient.getResult().isAllowed());

        PluginMessageEvent junk = message(p.on(lobby), ControlChannel.CHANNEL,
                "not a frame".getBytes(StandardCharsets.UTF_8));
        assertEq("a malformed frame is consumed", false, junk.getResult().isAllowed());
        assertEq("... and neither is answered", 0, p.pluginMessages.size());
        assertEq("... nor reaches felis-api", 0, api.count("GET " + SERVERS + "alpha/menu"));
    }

    private static void sources() {
        // A user backend is running its owner's plugins: refused, logged once a minute.
        Fakes.FakePlayer p = player(true);
        PluginMessageEvent e = message(p.on(beta), ControlChannel.CHANNEL,
                Control.encode(ControlFrame.wakeRequest(p.name, "alpha")));
        assertEq("user backend: consumed", false, e.getResult().isAllowed());
        message(p.on(beta), ControlChannel.CHANNEL, Control.encode(ControlFrame.wakeRequest(p.name, "alpha")));
        assertEq("user backend: logged once", 1, log.count("WARN", "refused felis:control 'WakeRequest' from backend beta"));
        assertEq("user backend: not woken", 0, api.count("POST " + SERVERS + "alpha/wake"));

        // LoginRelease belongs to the login gate alone.
        message(p.on(lobby), ControlChannel.CHANNEL, Control.encode(ControlFrame.loginRelease(p.name)));
        assertEq("lobby's LoginRelease: nothing moved", 0, p.connects.size());
        assertEq("lobby's LoginRelease: logged", 1, log.count("WARN", "refused felis:control 'LoginRelease' from backend lobby"));
        message(p.on(login), ControlChannel.CHANNEL, Control.encode(ControlFrame.loginRelease(p.name)));
        assertEq("login's LoginRelease: moved to the lobby", List.of("lobby"), List.copyOf(p.connects));

        // The lobby may only name managed user servers; a StatusQuery gets an answer
        // anyway so the tile stops loading, without echoing a malformed name.
        Fakes.FakePlayer q = player(true);
        message(q.on(lobby), ControlChannel.CHANNEL, Control.encode(ControlFrame.statusQuery("nope")));
        message(q.on(lobby), ControlChannel.CHANNEL, Control.encode(ControlFrame.statusQuery("lobby")));
        message(q.on(lobby), ControlChannel.CHANNEL, Control.encode(ControlFrame.statusQuery("../x")));
        message(q.on(lobby), ControlChannel.CHANNEL, Control.encode(ControlFrame.wakeRequest(q.name, "nope")));
        List<ControlFrame> r = frames(q);
        assertEq("bad server: three answers, none for the wake", 3, r.size());
        assertEq("unknown server", "Error not_found nope", brief(r.get(0)));
        assertEq("system server", "Error not_found lobby", brief(r.get(1)));
        assertEq("malformed name not echoed", "Error not_found null", brief(r.get(2)));
        assertEq("bad server: no felis-api call", 0, api.count("POST " + SERVERS + "nope/wake"));

        // The tile list: user servers only, sorted.
        message(q.on(lobby), ControlChannel.CHANNEL, Control.encode(ControlFrame.listRequest()));
        List<ControlFrame> all = frames(q);
        ControlFrame list = all.get(all.size() - 1);
        assertEq("list type", ControlFrame.LIST_UPDATE, list.type());
        assertEq("list: user servers, sorted", List.of("alpha", "beta", "gamma"), list.servers());
    }

    private static void statusQueries(ServerRegistry reg, FelisVelocityPlugin plugin) throws Exception {
        api.claimable.add("alpha");
        Fakes.FakePlayer p = player(true);
        message(p.on(lobby), ControlChannel.CHANNEL, Control.encode(ControlFrame.statusQuery("alpha")));
        Fakes.await("status answered", () -> p.pluginMessages.size() == 1);
        ControlFrame s = frames(p).get(0);
        assertEq("status type", ControlFrame.STATUS_UPDATE, s.type());
        assertEq("status fields", "alpha Stopped false 3/20 claimable=true",
                s.server() + " " + s.phase() + " " + s.ready() + " " + s.playersOnline() + "/" + s.playersMax()
                        + " claimable=" + s.claimable());

        // A second player within the TTL is answered from the shared projection, at once.
        Fakes.FakePlayer q = player(true);
        message(q.on(lobby), ControlChannel.CHANNEL, Control.encode(ControlFrame.statusQuery("alpha")));
        assertEq("shared answer is immediate", 1, q.pluginMessages.size());
        assertEq("shared answer is the same", "alpha Stopped false", brief3(frames(q).get(0)));
        assertEq("felis-api read once", 1, api.count("GET " + SERVERS + "alpha/menu"));

        // felis-api's own refusal comes back as its code and message.
        api.menuError.put("gamma", "404 not_found");
        message(p.on(lobby), ControlChannel.CHANNEL, Control.encode(ControlFrame.statusQuery("gamma")));
        Fakes.await("refusal answered", () -> p.pluginMessages.size() == 2);
        ControlFrame refused = frames(p).get(1);
        assertEq("refusal", "Error not_found gamma", brief(refused));
        assertEq("refusal message", "stub not_found", refused.message());

        // A transport failure never shows its internals (host, port) to the player.
        int closed;
        try (ServerSocket probe = new ServerSocket(0, 1, InetAddress.getLoopbackAddress())) {
            closed = probe.getLocalPort();
        }
        FelisApiClient dead = new FelisApiClient(new LinkConfig("http://127.0.0.1:" + closed, "t"));
        ControlChannel cut = new ControlChannel(net.proxy, log.logger, dead, router, reg, plugin, "login", "lobby");
        Fakes.FakePlayer t = player(true);
        cut.onPluginMessage(new PluginMessageEvent(t.on(lobby), t.player, ControlChannel.CHANNEL,
                Control.encode(ControlFrame.statusQuery("beta"))));
        Fakes.await("transport failure answered", () -> t.pluginMessages.size() == 1);
        ControlFrame down = frames(t).get(0);
        assertEq("transport failure code", "Error transport_error beta", brief(down));
        assertEq("transport failure message",
                "Felis 暂时不可用，请稍后再试 / Felis is temporarily unavailable — please try again.", down.message());
    }

    private static void claims() {
        // A refused claim answers with the refusal and never wakes.
        api.claimError.put("gamma", "403 quota_exceeded");
        Fakes.FakePlayer p = player(true);
        p.current = lobby;
        message(p.on(lobby), ControlChannel.CHANNEL, Control.encode(ControlFrame.claimRequest("someone-else", "gamma")));
        Fakes.await("claim refusal answered", () -> p.pluginMessages.size() == 1);
        assertEq("claim refused", "Error quota_exceeded gamma", brief(frames(p).get(0)));
        assertEq("claim refused: not woken", 0, api.count("POST " + SERVERS + "gamma/wake"));
        assertEq("the claim acted as the connection's player",
                true, api.bodies.get("POST " + SERVERS + "gamma/claim").contains(p.id.toString()));

        // A granted claim wakes and parks, and drops the stale "claimable" answer. Warm
        // the shared answer right before, so only the claim can have dropped it.
        Fakes.FakePlayer w = player(true);
        int reads = api.count("GET " + SERVERS + "alpha/menu");
        message(w.on(lobby), ControlChannel.CHANNEL, Control.encode(ControlFrame.statusQuery("alpha")));
        Fakes.await("warm status", () -> w.pluginMessages.size() == 1);
        int warm = api.count("GET " + SERVERS + "alpha/menu");
        assertEq("warmed (read at most once more)", true, warm == reads || warm == reads + 1);
        message(p.on(lobby), ControlChannel.CHANNEL, Control.encode(ControlFrame.claimRequest(p.name, "alpha")));
        Fakes.await("claimed server woken", () -> api.count("POST " + SERVERS + "alpha/wake") == 1);
        Fakes.await("claimer queued", () -> router.waitingCount() == 1);
        assertEq("the wake acted as the connection's player",
                true, api.bodies.get("POST " + SERVERS + "alpha/wake").contains(p.id.toString()));
        Fakes.FakePlayer q = player(true);
        message(q.on(lobby), ControlChannel.CHANNEL, Control.encode(ControlFrame.statusQuery("alpha")));
        Fakes.await("fresh status after the claim", () -> q.pluginMessages.size() == 1);
        assertEq("status re-read after the claim", warm + 1, api.count("GET " + SERVERS + "alpha/menu"));
    }

    private static void wakeAndTransfer() {
        // A WakeRequest parks the player; when the backend is ready the lobby hears
        // TransferReady just before the proxy moves them.
        Fakes.FakePlayer p = player(true);
        p.current = lobby;
        message(p.on(lobby), ControlChannel.CHANNEL, Control.encode(ControlFrame.wakeRequest("forged-name", "beta")));
        Fakes.await("beta woken", () -> api.count("POST " + SERVERS + "beta/wake") == 1);
        Fakes.await("waker queued", () -> router.waitingCount() == 2);
        api.ready.put("beta", true);
        router.tick();
        List<ControlFrame> r = frames(p);
        assertEq("one frame to the lobby", 1, r.size());
        assertEq("transfer ready", ControlFrame.TRANSFER_READY + " " + p.name + " beta",
                r.get(0).type() + " " + r.get(0).player() + " " + r.get(0).server());
        assertEq("then moved", List.of("beta"), List.copyOf(p.connects));
    }

    private static void budget() {
        // 96 frames in a burst, then 10 a second: a flood is cut off, the connection
        // is not. ListRequest is answered inline, so every accepted frame shows at once.
        Fakes.FakePlayer p = player(true);
        for (int i = 0; i < 150; i++) {
            message(p.on(lobby), ControlChannel.CHANNEL, Control.encode(ControlFrame.listRequest()));
        }
        int answered = p.pluginMessages.size();
        assertEq("flood cut at the burst (plus what refilled meanwhile): " + answered,
                true, answered >= 96 && answered <= 99);

        // Another player has a budget of their own.
        Fakes.FakePlayer q = player(true);
        message(q.on(lobby), ControlChannel.CHANNEL, Control.encode(ControlFrame.listRequest()));
        assertEq("other player unaffected", 1, q.pluginMessages.size());

        // Leaving the proxy forgets the budget: a fresh burst, far more than a refill.
        channel.onDisconnect(new DisconnectEvent(p.player, DisconnectEvent.LoginStatus.SUCCESSFUL_LOGIN));
        p.pluginMessages.clear();
        for (int i = 0; i < 50; i++) {
            message(p.on(lobby), ControlChannel.CHANNEL, Control.encode(ControlFrame.listRequest()));
        }
        assertEq("budget forgotten on disconnect", 50, p.pluginMessages.size());
    }

    // ---- helpers ----

    private static Fakes.FakePlayer player(boolean linked) {
        int n = nextPlayer++;
        Fakes.FakePlayer p = new Fakes.FakePlayer(net, "player" + n, n);
        if (linked) {
            api.linked.add(p.id);
        }
        return p;
    }

    // message delivers a frame a backend sent up its connection toward its player.
    private static PluginMessageEvent message(ServerConnection source, ChannelIdentifier id, byte[] data) {
        PluginMessageEvent e = new PluginMessageEvent(source, source.getPlayer(), id, data);
        channel.onPluginMessage(e);
        return e;
    }

    private static List<ControlFrame> frames(Fakes.FakePlayer p) {
        List<ControlFrame> out = new ArrayList<>();
        synchronized (p.pluginMessages) {
            for (byte[] b : p.pluginMessages) {
                out.add(Control.decode(b));
            }
        }
        return out;
    }

    private static String brief(ControlFrame f) {
        return f.type() + " " + f.code() + " " + f.server();
    }

    private static String brief3(ControlFrame f) {
        return f.server() + " " + f.phase() + " " + f.ready();
    }

    private static ServerView view(String name, boolean ready, String addr) {
        return new ServerView(name, name, ready ? "Running" : "Stopped", ready, "ownerOnly",
                "Running", "ClusterIP", addr, 0, 20);
    }

    private static void assertEq(String what, Object want, Object got) {
        if (want == null ? got != null : !want.equals(got)) {
            throw new AssertionError(what + ": got " + got + ", want " + want);
        }
        checks++;
    }
}
