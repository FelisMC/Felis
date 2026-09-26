package best.lolicon.felis.velocity;

import best.lolicon.felis.link.FelisApiClient;
import best.lolicon.felis.link.LinkConfig;
import best.lolicon.felis.link.ServerView;

import com.velocitypowered.api.event.Continuation;
import com.velocitypowered.api.event.EventTask;
import com.velocitypowered.api.event.connection.DisconnectEvent;
import com.velocitypowered.api.event.player.PlayerChooseInitialServerEvent;
import com.velocitypowered.api.event.player.ServerConnectedEvent;
import com.velocitypowered.api.event.player.ServerPreConnectEvent;
import com.velocitypowered.api.proxy.server.RegisteredServer;

import java.nio.file.Files;
import java.util.ArrayList;
import java.util.Collections;
import java.util.List;
import java.util.Locale;

/**
 * WaitingRouterTest drives the real WaitingRouter, ServerRegistry, FelisApiClient and
 * FelisVelocityPlugin call pool through Velocity's own event objects, against a fake
 * proxy and a stub felis-api: where a connecting host sends a player, what the login
 * gate may release them to and when, how each wake refusal is told apart, how the
 * waiting queue is drained (one status poll per server per tick, only linked players
 * moved), and which joins are reported. Framework free: a failed assertion throws.
 *
 * <p>Not covered here: the 120 s queue timeout (the router reads the wall clock) and a
 * real client's timing, which needs an online-mode account on a live stack.
 *
 * <p>Run: {@code ./gradlew routingTest} in plugins/velocity.
 */
public final class WaitingRouterTest {

    private static final String LINK = "GET /api/v1/internal/account/link/status/";
    private static final String SERVERS = "/api/v1/internal/servers/";

    private static int checks;
    private static int nextPlayer = 1;

    private static Fakes.Net net;
    private static Fakes.Log log;
    private static Fakes.Api api;
    private static ServerRegistry reg;
    private static FelisVelocityPlugin plugin;
    private static WaitingRouter router;
    private static RegisteredServer login;
    private static RegisteredServer lobby;
    private static RegisteredServer beta;

    public static void main(String[] args) throws Exception {
        net = new Fakes.Net();
        log = new Fakes.Log();
        api = new Fakes.Api();
        try {
            FelisApiClient client = new FelisApiClient(new LinkConfig(api.url(), "t"));
            reg = new ServerRegistry(net.proxy, log.logger, "mc.test");
            reg.refresh(servers(true));
            plugin = new FelisVelocityPlugin(net.proxy, log.logger, Files.createTempDirectory("felis-routing"));
            router = new WaitingRouter(net.proxy, log.logger, client, reg, plugin, "login", "lobby");
            login = net.proxy.getServer("login").orElseThrow();
            lobby = net.proxy.getServer("lobby").orElseThrow();
            beta = net.proxy.getServer("beta").orElseThrow();
            // A stopped server's address field names its fallback; dialled, "login"
            // resolves nowhere.
            assertEq("a stopped server is not registered at its fallback's name", null, net.address("alpha"));

            hostRouting();
            loginGate();
            wakeRefusals();
            queue();
            menuAndCommands();
            joins();
            disconnectAndRelease();
        } finally {
            api.close();
        }
        System.out.println("WaitingRouterTest OK (" + checks + " checks)");
    }

    // The server list the stub control plane serves. "gone" is dropped by a later
    // refresh; "fresh" has no backend address yet.
    private static List<ServerView> servers(boolean withGone) {
        List<ServerView> list = new ArrayList<>(List.of(
                view("login", true, "10.43.0.1:25565"),
                view("lobby", true, "10.43.0.2:25565"),
                view("alpha", false, "10.43.0.3:25565"),
                view("beta", true, "10.43.0.4:25565"),
                view("gamma", false, "10.43.0.5:25565"),
                view("delta", false, "10.43.0.6:25565"),
                view("epsilon", false, "10.43.0.7:25565"),
                view("zeta", false, "10.43.0.8:25565"),
                view("fresh", false, null)));
        if (withGone) {
            list.add(view("gone", false, "10.43.0.9:25565"));
        }
        return list;
    }

    private static void hostRouting() {
        // A sleeping server: the player lands on login, and the linked release goes to
        // the lobby while the server is woken and the player queued.
        Fakes.FakePlayer p1 = player("alpha.mc.test", true);
        assertEq("fresh connection goes to login", "login", choose(p1));
        ServerPreConnectEvent e = release(p1);
        assertEq("sleeping target: released to the lobby", "lobby", allowedTo(e));
        assertEq("sleeping target: woken once", 1, api.count("POST " + SERVERS + "alpha/wake"));
        assertEq("sleeping target: told", true, p1.said("Starting « alpha »"));
        assertEq("sleeping target: queued", 1, router.waitingCount());
        assertEq("the release checked the link", 1, api.count(LINK + p1.id));

        // A ready server: the release goes straight to it, no wake; until the connect
        // lands, a repeated release (the gate retries) goes there again.
        Fakes.FakePlayer p2 = player("BETA.mc.test", true);
        choose(p2);
        assertEq("ready target: released to it", "beta", allowedTo(release(p2)));
        assertEq("ready target: retried release still goes to it", "beta", allowedTo(release(p2)));
        assertEq("ready target: no wake", 0, api.count("POST " + SERVERS + "beta/wake"));
        router.onServerConnected(new ServerConnectedEvent(p2.player, beta, login));
        assertEq("after the connect the target is spent", "lobby", allowedTo(release(p2)));

        // login.<root> is the gate, never a destination: remembering it would loop.
        Fakes.FakePlayer p3 = player("login.mc.test", true);
        choose(p3);
        assertEq("login host: released to the lobby", "lobby", allowedTo(release(p3)));
        assertEq("login host: nothing woken", 0, api.count("POST " + SERVERS + "login/wake"));

        // lobby.<root> is where the release goes anyway; even a lobby felis-api reports
        // as not ready is never woken through the queue.
        List<ServerView> lobbyDown = servers(true);
        lobbyDown.set(1, view("lobby", false, "10.43.0.2:25565"));
        reg.refresh(lobbyDown);
        Fakes.FakePlayer p4 = player("lobby.mc.test", true);
        choose(p4);
        assertEq("lobby host: released to the lobby", "lobby", allowedTo(release(p4)));
        assertEq("lobby host: not woken", 0, api.count("POST " + SERVERS + "lobby/wake"));
        assertEq("lobby host: not queued", 1, router.waitingCount());
        reg.refresh(servers(true));

        // Hosts that name no server fall through to the lobby.
        for (String host : new String[]{"nope.mc.test", "alpha.evil.test", "mc.test"}) {
            Fakes.FakePlayer p = player(host, true);
            assertEq(host + ": still login first", "login", choose(p));
            assertEq(host + ": released to the lobby", "lobby", allowedTo(release(p)));
        }
        assertEq("no stray wake from other hosts", 1, api.count("POST " + SERVERS + "alpha/wake"));

        // A reconnect never inherits the host of an earlier connection.
        Fakes.FakePlayer p5 = player("alpha.mc.test", true);
        choose(p5);
        p5.virtualHost = null;
        choose(p5);
        assertEq("reconnect: released to the lobby", "lobby", allowedTo(release(p5)));
        assertEq("reconnect: nothing woken", 1, api.count("POST " + SERVERS + "alpha/wake"));

        // A target that vanished between connect and release.
        Fakes.FakePlayer p6 = player("gone.mc.test", true);
        choose(p6);
        reg.refresh(servers(false));
        assertEq("vanished target: released to the lobby", "lobby", allowedTo(release(p6)));
        assertEq("vanished target: told", true, p6.said("« gone » is no longer available"));
        assertEq("vanished target: not woken", 0, api.count("POST " + SERVERS + "gone/wake"));
    }

    private static void loginGate() {
        Fakes.FakePlayer p = player(null, true);

        // Only a move out of login is gated.
        ServerPreConnectEvent first = new ServerPreConnectEvent(p.player, beta);
        assertEq("initial connect is not gated", null, router.onServerPreConnect(first));
        assertEq("... and keeps its target", "beta", allowedTo(first));
        ServerPreConnectEvent fromLobby = new ServerPreConnectEvent(p.player, beta, lobby);
        assertEq("a move from the lobby is not gated", null, router.onServerPreConnect(fromLobby));
        assertEq("... and keeps its target", "beta", allowedTo(fromLobby));

        // The gate may only release to the lobby, linked or not.
        Fakes.FakePlayer zh = player(null, true);
        zh.locale = Locale.SIMPLIFIED_CHINESE;
        ServerPreConnectEvent elsewhere = new ServerPreConnectEvent(zh.player, beta, login);
        assertEq("login to a user server: decided at once", null, router.onServerPreConnect(elsewhere));
        assertEq("login to a user server: denied", false, elsewhere.getResult().isAllowed());
        assertEq("login to a user server: told (zh)", true, zh.said("登录网关只能把玩家放行到大厅。"));
        assertEq("login to a user server: logged", 1, log.count("WARN", "denied login-gate transfer for " + zh.id));
        assertEq("login to a user server: no link lookup", 0, api.count(LINK + zh.id));

        // Unlinked: denied, and the gate's retries do not repeat the line.
        Fakes.FakePlayer unlinked = player(null, false);
        assertEq("unlinked: denied", false, release(unlinked).getResult().isAllowed());
        assertEq("unlinked: denied again", false, release(unlinked).getResult().isAllowed());
        assertEq("unlinked: told once", 1, count(unlinked.messages, "Finish signing in before leaving"));
        assertEq("unlinked: both asked felis-api", 2, api.count(LINK + unlinked.id));

        // felis-api down and no recent confirmation: fail closed.
        Fakes.FakePlayer unknown = player(null, true);
        api.linkDown = true;
        assertEq("api down, never confirmed: denied", false, release(unknown).getResult().isAllowed());
        assertEq("api down, never confirmed: told", true, unknown.said("temporarily unavailable"));
        assertEq("api down, never confirmed: logged", 1,
                log.count("WARN", "could not verify login release for " + unknown.id));
        api.linkDown = false;

        // felis-api down after a confirmation minutes ago: admitted, and said so.
        Fakes.FakePlayer known = player(null, true);
        assertEq("confirmed: released", "lobby", allowedTo(release(known)));
        api.linkDown = true;
        assertEq("api down, recently confirmed: released", "lobby", allowedTo(release(known)));
        assertEq("api down, recently confirmed: logged", 1, log.count("WARN", "admitting " + known.id));
        api.linkDown = false;
    }

    private static void wakeRefusals() {
        String[][] cases = {
                {"403 forbidden", "You're not allowed to start « gamma »", null},
                {"409 maintenance_in_progress", "« gamma » is under maintenance", null},
                {"409 conflict", "Couldn't start « gamma » right now", "wake gamma failed (status=409)"},
                {"503 at_capacity", "The cluster is at capacity right now", null},
                {"503 unavailable", "Couldn't start « gamma » right now", "wake gamma failed (status=503)"},
                {"500 internal", "Couldn't start « gamma » right now", "wake gamma failed (status=500)"},
        };
        for (String[] c : cases) {
            api.wakeError.put("gamma", c[0]);
            int before = router.waitingCount();
            int warned = c[2] == null ? 0 : log.count("WARN", c[2]);
            Fakes.FakePlayer p = player("gamma.mc.test", true);
            choose(p);
            assertEq(c[0] + ": released to the lobby", "lobby", allowedTo(release(p)));
            assertEq(c[0] + ": told", true, p.said(c[1]));
            assertEq(c[0] + ": not queued", before, router.waitingCount());
            assertEq(c[0] + ": no promise of a start", false, p.said("Starting « gamma »"));
            if (c[2] != null) {
                assertEq(c[0] + ": logged", warned + 1, log.count("WARN", c[2]));
            }
        }
        // 429: a wake is already in flight, so join the wait.
        api.wakeError.put("gamma", "429 cooldown");
        int before = router.waitingCount();
        Fakes.FakePlayer p = player("gamma.mc.test", true);
        choose(p);
        release(p);
        assertEq("429: queued", before + 1, router.waitingCount());
        assertEq("429: told it is starting", true, p.said("Starting « gamma »"));
        api.wakeError.remove("gamma");
    }

    private static void queue() {
        // Queue now: one player on alpha (hostRouting), one on gamma (the 429).
        Fakes.FakePlayer onAlpha = player("alpha.mc.test", true);
        choose(onAlpha);
        release(onAlpha);
        assertEq("queue holds three", 3, router.waitingCount());
        List<Fakes.FakePlayer> alphaWaiters = List.of(onAlpha);

        int alphaPolls = api.count("GET " + SERVERS + "alpha/status");
        int gammaPolls = api.count("GET " + SERVERS + "gamma/status");
        router.tick();
        assertEq("one status poll per server, however many wait on it", alphaPolls + 1,
                api.count("GET " + SERVERS + "alpha/status"));
        assertEq("gamma polled too", gammaPolls + 1, api.count("GET " + SERVERS + "gamma/status"));
        assertEq("nothing ready: nobody moved", 0, onAlpha.connects.size());
        assertEq("nothing ready: all still queued", 3, router.waitingCount());

        api.ready.put("alpha", true);
        router.tick();
        assertEq("alpha ready: registered from the poll, not the next refresh", "10.43.0.3:25565",
                net.address("alpha"));
        assertEq("alpha ready: its waiters moved", List.of("alpha"), List.copyOf(onAlpha.connects));
        assertEq("alpha ready: told", true, onAlpha.said("« alpha » is ready — moving you in"));
        assertEq("alpha ready: one poll again", alphaPolls + 2, api.count("GET " + SERVERS + "alpha/status"));
        assertEq("alpha ready: only gamma's waiter left", 1, router.waitingCount());
        for (Fakes.FakePlayer w : alphaWaiters) {
            assertEq("moved player checked linked again", true, api.count(LINK + w.id) >= 2);
        }

        // A waiter who left the proxy is dropped without polling for them.
        net.players.clear();
        int polls = api.count("GET " + SERVERS + "gamma/status");
        router.tick();
        assertEq("departed waiter dropped", 0, router.waitingCount());
        assertEq("departed waiter: no poll", polls, api.count("GET " + SERVERS + "gamma/status"));

        // Unlinked by the time the server is ready: dropped and told, not moved.
        Fakes.FakePlayer lapsed = player("delta.mc.test", true);
        choose(lapsed);
        release(lapsed);
        assertEq("lapsed: queued", 1, router.waitingCount());
        api.linked.remove(lapsed.id);
        api.ready.put("delta", true);
        router.tick();
        assertEq("lapsed: dropped", 0, router.waitingCount());
        assertEq("lapsed: not moved", 0, lapsed.connects.size());
        assertEq("lapsed: told", true, lapsed.said("no longer linked"));

        // alpha stops: the fallback report leaves its last registration alone. It comes
        // back behind a new address, and the waiter goes there, not to the old one.
        api.ready.put("alpha", false);
        reg.refresh(servers(false));
        assertEq("stopped: last direct registration kept", "10.43.0.3:25565", net.address("alpha"));
        Fakes.FakePlayer back = player("alpha.mc.test", true);
        choose(back);
        release(back);
        assertEq("stopped again: queued", 1, router.waitingCount());
        api.address.put("alpha", "10.43.0.13:25565");
        api.ready.put("alpha", true);
        router.tick();
        assertEq("new address: re-registered from the poll", "10.43.0.13:25565", net.address("alpha"));
        assertEq("new address: moved", List.of("alpha"), List.copyOf(back.connects));
        assertEq("new address: queue empty", 0, router.waitingCount());

        // Ready, but neither the registry nor the poll has an address for it yet: wait
        // for the refresh that brings one.
        Fakes.FakePlayer early = player("fresh.mc.test", true);
        choose(early);
        release(early);
        api.ready.put("fresh", true);
        router.tick();
        assertEq("unregistered: still queued", 1, router.waitingCount());
        assertEq("unregistered: not moved", 0, early.connects.size());
        List<ServerView> list = servers(false);
        list.set(list.size() - 1, view("fresh", true, "10.43.0.10:25565"));
        reg.refresh(list);
        router.tick();
        assertEq("registered: moved", List.of("fresh"), List.copyOf(early.connects));
        assertEq("registered: queue empty", 0, router.waitingCount());
    }

    private static void menuAndCommands() {
        List<String> notified = Collections.synchronizedList(new ArrayList<>());
        router.setMenuTransferListener((player, server) -> notified.add(player.getUsername() + "@" + server));

        Fakes.FakePlayer menu = player(null, true);
        menu.current = lobby;
        Fakes.FakePlayer command = player(null, true);
        command.current = lobby;
        router.enqueueFromMenu(menu.player, "epsilon");
        router.enqueueFromCommand(command.player, "epsilon");
        Fakes.await("both queued", () -> router.waitingCount() == 2);
        assertEq("each entry woke epsilon", 2, api.count("POST " + SERVERS + "epsilon/wake"));
        api.ready.put("epsilon", true);
        router.tick();
        assertEq("both moved (menu)", List.of("epsilon"), List.copyOf(menu.connects));
        assertEq("both moved (command)", List.of("epsilon"), List.copyOf(command.connects));
        assertEq("only the menu entry tells the lobby", List.of(menu.name + "@epsilon"), List.copyOf(notified));

        // Asking for the server you stand on is answered at once, case-insensitively.
        Fakes.FakePlayer there = player(null, true);
        there.current = beta;
        router.enqueueFromCommand(there.player, "BETA");
        assertEq("already there: told", true, there.said("You're already on « BETA »"));
        assertEq("already there: nothing asked", 0, api.count(LINK + there.id));

        // An invite to a running server joins it; to a stopped one it goes through the
        // policy-gated wake like any other entry.
        Fakes.FakePlayer invited = player(null, true);
        invited.current = lobby;
        router.enqueueFromInvite(invited.player, "beta");
        Fakes.await("invitee moved", () -> invited.connects.size() == 1);
        assertEq("invite to a running server: joined", List.of("beta"), List.copyOf(invited.connects));
        assertEq("invite to a running server: no wake", 0, api.count("POST " + SERVERS + "beta/wake"));

        Fakes.FakePlayer invitedStopped = player(null, true);
        invitedStopped.current = lobby;
        router.enqueueFromInvite(invitedStopped.player, "zeta");
        Fakes.await("zeta woken", () -> api.count("POST " + SERVERS + "zeta/wake") == 1);
        Fakes.await("invitee queued", () -> router.waitingCount() == 1);
        assertEq("invite to a stopped server: not moved yet", 0, invitedStopped.connects.size());

        Fakes.FakePlayer unlinked = player(null, false);
        unlinked.current = lobby;
        router.enqueueFromCommand(unlinked.player, "zeta");
        Fakes.await("unlinked told", () -> unlinked.said("Finish signing in before joining a server"));
        assertEq("unlinked command: no wake", 1, api.count("POST " + SERVERS + "zeta/wake"));
        assertEq("unlinked command: not queued", 1, router.waitingCount());
    }

    private static void joins() throws InterruptedException {
        Fakes.FakePlayer p = player(null, true);
        RegisteredServer alpha = net.proxy.getServer("alpha").orElseThrow();
        RegisteredServer stat = net.add("static", "10.0.0.50", 25565);
        router.onServerConnected(new ServerConnectedEvent(p.player, lobby, null));
        router.onServerConnected(new ServerConnectedEvent(p.player, login, null));
        router.onServerConnected(new ServerConnectedEvent(p.player, stat, lobby));
        router.onServerConnected(new ServerConnectedEvent(p.player, alpha, lobby));
        Fakes.await("alpha join reported", () -> api.count("POST " + SERVERS + "alpha/join-event") == 1);
        Thread.sleep(200); // let anything wrongly submitted for the others land too
        assertEq("lobby join not reported", 0, api.count("POST " + SERVERS + "lobby/join-event"));
        assertEq("login join not reported", 0, api.count("POST " + SERVERS + "login/join-event"));
        assertEq("unmanaged join not reported", 0, api.count("POST " + SERVERS + "static/join-event"));
        assertEq("no failure counted", 0L, plugin.stats().total(ProxyStats.Event.JOIN_EVENT_FAILED));

        api.joinStatus = 500;
        router.onServerConnected(new ServerConnectedEvent(p.player, beta, lobby));
        Fakes.await("failed join counted", () -> plugin.stats().total(ProxyStats.Event.JOIN_EVENT_FAILED) == 1);
        Fakes.await("failed join logged",
                () -> log.count("WARN", "join-event for " + p.id + " on beta failed (status=500)") == 1);
        api.joinStatus = 204;

        // A transfer the backend refuses is counted, logged and told.
        Fakes.FakePlayer refused = player(null, true);
        refused.current = lobby;
        refused.connectSucceeds = false;
        router.enqueueFromInvite(refused.player, "beta");
        Fakes.await("failed transfer counted", () -> plugin.stats().total(ProxyStats.Event.TRANSFER_FAILED) == 1);
        assertEq("failed transfer: told", true, refused.said("Couldn't connect you to « beta »"));
        assertEq("failed transfer: logged", 1, log.count("WARN", "transfer of " + refused.id + " to beta failed"));
    }

    private static void disconnectAndRelease() {
        // Leaving the proxy drops the queue entry.
        Fakes.FakePlayer leaver = player(null, true);
        leaver.current = lobby;
        int before = router.waitingCount();
        router.enqueueFromCommand(leaver.player, "zeta");
        Fakes.await("leaver queued", () -> router.waitingCount() == before + 1);
        router.onDisconnect(new DisconnectEvent(leaver.player, DisconnectEvent.LoginStatus.SUCCESSFUL_LOGIN));
        assertEq("disconnect drops the entry", before, router.waitingCount());

        // The login gate's release asks Velocity for the lobby.
        Fakes.FakePlayer released = player(null, true);
        router.releaseFromLogin(released.player);
        assertEq("release connects to the lobby", List.of("lobby"), List.copyOf(released.connects));

        net.remove("lobby");
        Fakes.FakePlayer early = player(null, true);
        router.releaseFromLogin(early.player);
        assertEq("no lobby yet: nothing sent", 0, early.connects.size());
        assertEq("no lobby yet: logged", 1, log.count("WARN", "login release for " + early.id + " but the lobby"));

        // With no login gate registered a host-routed player is turned away.
        net.remove("login");
        Fakes.FakePlayer stranded = player("beta.mc.test", true);
        PlayerChooseInitialServerEvent e = new PlayerChooseInitialServerEvent(stranded.player, null);
        router.onChooseInitialServer(e);
        assertEq("no login: no initial server", false, e.getInitialServer().isPresent());
        assertEq("no login: disconnected with a reason", "The Felis login gate is unavailable. Please reconnect shortly.",
                stranded.disconnectedWith);
    }

    // ---- helpers ----

    private static Fakes.FakePlayer player(String host, boolean linked) {
        int n = nextPlayer++;
        Fakes.FakePlayer p = new Fakes.FakePlayer(net, "player" + n, n);
        p.virtualHost = host;
        if (linked) {
            api.linked.add(p.id);
        }
        return p;
    }

    // choose runs PlayerChooseInitialServerEvent the way Velocity does (the first
    // try server, login, preset) and returns the server the player will land on.
    private static String choose(Fakes.FakePlayer p) {
        PlayerChooseInitialServerEvent e = new PlayerChooseInitialServerEvent(p.player, login);
        router.onChooseInitialServer(e);
        return e.getInitialServer().map(s -> s.getServerInfo().getName()).orElse(null);
    }

    // release is the login gate asking to move the player to the lobby, with the
    // router's async part run to completion the way Velocity's event manager would.
    private static ServerPreConnectEvent release(Fakes.FakePlayer p) {
        p.current = login;
        ServerPreConnectEvent e = new ServerPreConnectEvent(p.player, lobby, login);
        EventTask task = router.onServerPreConnect(e);
        if (task != null) {
            task.execute(new Continuation() {
                @Override
                public void resume() {
                }

                @Override
                public void resumeWithException(Throwable t) {
                    throw new AssertionError("pre-connect task failed", t);
                }
            });
        }
        return e;
    }

    private static String allowedTo(ServerPreConnectEvent e) {
        if (!e.getResult().isAllowed()) {
            return "denied";
        }
        return e.getResult().getServer().map(s -> s.getServerInfo().getName()).orElse(null);
    }

    private static int count(List<String> lines, String part) {
        int n = 0;
        synchronized (lines) {
            for (String l : lines) {
                if (l.contains(part)) {
                    n++;
                }
            }
        }
        return n;
    }

    // view is a server as GET /servers lists it: up at its direct address, or down on
    // the fallback, where the operator writes the fallback server's name ("login")
    // into the address. addr is also what the stub API reports once the server is up.
    private static ServerView view(String name, boolean ready, String addr) {
        if (addr != null) {
            api.address.put(name, addr);
        }
        return new ServerView(name, name, ready ? "Running" : "Stopped", ready, "ownerOnly",
                "Running", ready ? "direct" : "fallback", ready ? addr : "login", 0, 20);
    }

    private static void assertEq(String what, Object want, Object got) {
        if (want == null ? got != null : !want.equals(got)) {
            throw new AssertionError(what + ": got " + got + ", want " + want);
        }
        checks++;
    }
}
