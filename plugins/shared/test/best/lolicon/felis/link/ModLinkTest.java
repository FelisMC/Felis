package best.lolicon.felis.link;

import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;

import java.io.IOException;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.List;
import java.util.UUID;

/**
 * ModLinkTest runs the mods' shared {@code /link} ({@link ModLink}) the way a mod
 * does, with a fake command source, a hand-cranked I/O thread and server thread, and
 * the real LinkClient against a stub felis-api. It checks what the caller gets: the
 * console and an offline-mode server are refused before anything is minted, a player
 * hears "requesting" at once while the code is fetched off the server thread, the
 * code and where to redeem it (the minted panel URL, else the console) arrive back on
 * the server thread, and any failure is one generic line with the details in the log.
 *
 * <p>Run: {@code javac -d <out> shared/src/main/java/best/lolicon/felis/link/*.java
 * shared/test/best/lolicon/felis/link/ModLinkTest.java && java -cp <out>
 * best.lolicon.felis.link.ModLinkTest} (plugins/test.sh does).
 */
public final class ModLinkTest {

    private static final String CODE = "K7Q2-9XPM";
    private static int checks;

    // The stub's knobs and what it saw.
    private static volatile int status = 201;
    private static volatile String panelUrl;
    private static final List<String> bodies = new ArrayList<>();

    public static void main(String[] args) throws Exception {
        HttpServer stub = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        stub.createContext("/", ModLinkTest::handle);
        stub.start();
        String base = "http://127.0.0.1:" + stub.getAddress().getPort();
        try {
            LinkClient client = new LinkClient(new LinkConfig(base, "mod-token"));
            consoleIsRefused(client);
            offlineModeIsRefused(client);
            playerGetsTheCodeAndThePanelUrl(client);
            codeWithoutPanelUrlPointsAtTheConsole(client);
            refusalIsOneGenericLine(client);
        } finally {
            stub.stop(0);
        }
        felisApiDownIsOneGenericLine(new LinkClient(new LinkConfig(base, "mod-token")));
        System.out.println("ModLinkTest OK (" + checks + " checks)");
    }

    private static void consoleIsRefused(LinkClient client) {
        Rig rig = new Rig(client);
        Caller console = new Caller(null, true);
        assertEq("the console's /link is refused", 0, rig.link.run(console));
        assertEq("... as players-only", List.of(ModLink.PLAYERS_ONLY), console.failures);
        assertEq("... and nothing is fetched", 0, rig.io.size());
    }

    private static void offlineModeIsRefused(LinkClient client) {
        Rig rig = new Rig(client);
        Caller player = new Caller(UUID.randomUUID(), false);
        assertEq("/link on an offline-mode server is refused", 0, rig.link.run(player));
        assertEq("... with the reason", List.of(ModLink.OFFLINE_REPLY), player.failures);
        assertEq("... logged", List.of(ModLink.OFFLINE_LOG), rig.warnings);
        assertEq("... and no code minted for a UUID nobody checked", 0, rig.io.size());
        assertEq("... nor a line told", List.of(), player.told);
    }

    private static void playerGetsTheCodeAndThePanelUrl(LinkClient client) {
        Rig rig = new Rig(client);
        panelUrl = "https://panel.example.test/link";
        UUID id = UUID.randomUUID();
        Caller player = new Caller(id, true);
        int before = bodies.size();
        assertEq("/link runs", 1, rig.link.run(player));
        assertEq("the player hears at once that the code is coming", List.of(ModLink.REQUESTING), player.told);
        assertEq("... and felis-api is not called on the server thread", before, bodies.size());
        rig.runIo(player);
        assertEq("the I/O thread mints one code", before + 1, bodies.size());
        assertTrue("... for this player's UUID", bodies.get(before).contains(id.toString()));
        assertEq("... and tells nothing itself", List.of(ModLink.REQUESTING), player.told);
        player.drainServerThread();
        assertEq("back on the server thread: the code, then the panel URL",
                List.of(ModLink.REQUESTING, ModLink.codeLine(new LinkCode(CODE, null, panelUrl)),
                        "在此完成绑定 / Finish linking at: " + panelUrl),
                player.told);
        assertTrue("the code line shows the code", player.told.get(1).contains(CODE));
        assertEq("nothing logged", List.of(), rig.warnings);
        panelUrl = null;
    }

    private static void codeWithoutPanelUrlPointsAtTheConsole(LinkClient client) {
        Rig rig = new Rig(client);
        Caller player = new Caller(UUID.randomUUID(), true);
        rig.link.run(player);
        rig.runIo(player);
        player.drainServerThread();
        assertEq("an older felis-api's code points at the console", ModLink.CONSOLE_HINT, last(player.told));
    }

    private static void refusalIsOneGenericLine(LinkClient client) {
        Rig rig = new Rig(client);
        status = 429;
        UUID id = UUID.randomUUID();
        Caller player = new Caller(id, true);
        rig.link.run(player);
        rig.runIo(player);
        player.drainServerThread();
        assertEq("a refusal: requesting, then one generic line", List.of(ModLink.REQUESTING, ModLink.FAILED), player.told);
        assertEq("... logged once", 1, rig.warnings.size());
        String log = rig.warnings.get(0);
        assertTrue("... naming the player, the status and felis-api's code: " + log,
                log.contains(id.toString()) && log.contains("status=429") && log.contains("code=rate_limited"));
        assertTrue("... while the player never sees felis-api's words", !String.join(" ", player.told).contains("slow down"));
        status = 201;
    }

    private static void felisApiDownIsOneGenericLine(LinkClient client) {
        Rig rig = new Rig(client);
        Caller player = new Caller(UUID.randomUUID(), true);
        rig.link.run(player);
        rig.runIo(player);
        player.drainServerThread();
        assertEq("felis-api unreachable: the same generic line", ModLink.FAILED, last(player.told));
        assertTrue("... logged as a transport failure", rig.warnings.get(0).contains("status=0"));
    }

    // ---- rig ----

    private static final class Rig {
        final List<Runnable> io = new ArrayList<>();
        final List<String> warnings = new ArrayList<>();
        final ModLink link;

        Rig(LinkClient client) {
            link = new ModLink(client, io::add, warnings::add);
        }

        /** runIo runs the queued felis-api calls, off the caller's server thread. */
        void runIo(Caller caller) {
            caller.onServerThread = false;
            List<Runnable> queued = new ArrayList<>(io);
            io.clear();
            queued.forEach(Runnable::run);
            caller.onServerThread = true;
        }
    }

    /** Caller is a command source: a player (or the console), and what it was told. */
    private static final class Caller implements ModLink.Source {
        final UUID id;
        final boolean online;
        final List<String> failures = new ArrayList<>();
        final List<String> told = new ArrayList<>();
        final List<Runnable> serverThread = new ArrayList<>();
        boolean onServerThread = true;

        Caller(UUID id, boolean online) {
            this.id = id;
            this.online = online;
        }

        void drainServerThread() {
            List<Runnable> queued = new ArrayList<>(serverThread);
            serverThread.clear();
            queued.forEach(Runnable::run);
        }

        @Override
        public UUID player() {
            return id;
        }

        @Override
        public boolean onlineMode() {
            return online;
        }

        @Override
        public void fail(String line) {
            failures.add(line);
        }

        @Override
        public void tell(String line) {
            if (!onServerThread) {
                throw new AssertionError("told the player off the server thread: " + line);
            }
            told.add(line);
        }

        @Override
        public void onServerThread(Runnable task) {
            serverThread.add(task);
        }
    }

    private static void handle(HttpExchange ex) throws IOException {
        String body = new String(ex.getRequestBody().readAllBytes(), StandardCharsets.UTF_8);
        if (!"Bearer mod-token".equals(ex.getRequestHeaders().getFirst("Authorization"))
                || !ex.getRequestURI().getPath().endsWith("/account/link/code")) {
            reply(ex, 404, "{\"error\":{\"code\":\"not_found\",\"message\":\"not_found\"}}");
            return;
        }
        synchronized (bodies) {
            bodies.add(body);
        }
        if (status != 201) {
            reply(ex, status, "{\"error\":{\"code\":\"rate_limited\",\"message\":\"slow down\"}}");
            return;
        }
        String url = panelUrl;
        reply(ex, 201, "{\"code\":\"" + CODE + "\",\"expires_at\":\"2026-09-27T12:10:00Z\""
                + (url != null ? ",\"panel_url\":\"" + url + "\"" : "") + "}");
    }

    private static void reply(HttpExchange ex, int code, String body) throws IOException {
        byte[] bytes = body.getBytes(StandardCharsets.UTF_8);
        ex.getResponseHeaders().set("Content-Type", "application/json");
        ex.sendResponseHeaders(code, bytes.length);
        try (OutputStream os = ex.getResponseBody()) {
            os.write(bytes);
        }
    }

    private static <T> T last(List<T> list) {
        return list.isEmpty() ? null : list.get(list.size() - 1);
    }

    private static void assertTrue(String what, boolean ok) {
        if (!ok) {
            throw new AssertionError(what);
        }
        checks++;
    }

    private static void assertEq(String what, Object want, Object got) {
        if (want == null ? got != null : !want.equals(got)) {
            throw new AssertionError(what + ": got " + got + ", want " + want);
        }
        checks++;
    }
}
