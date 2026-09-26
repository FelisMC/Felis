package best.lolicon.felis.velocity;

import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;
import com.velocitypowered.api.proxy.ConnectionRequestBuilder;
import com.velocitypowered.api.proxy.Player;
import com.velocitypowered.api.proxy.ProxyServer;
import com.velocitypowered.api.proxy.ServerConnection;
import com.velocitypowered.api.proxy.messages.ChannelIdentifier;
import com.velocitypowered.api.proxy.messages.ChannelRegistrar;
import com.velocitypowered.api.proxy.player.PlayerSettings;
import com.velocitypowered.api.proxy.server.RegisteredServer;
import com.velocitypowered.api.proxy.server.ServerInfo;
import net.kyori.adventure.text.Component;
import net.kyori.adventure.text.TextComponent;
import org.slf4j.Logger;

import java.io.IOException;
import java.io.OutputStream;
import java.lang.reflect.InvocationHandler;
import java.lang.reflect.Proxy;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.Collections;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.Optional;
import java.util.Set;
import java.util.UUID;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.ConcurrentHashMap;
import java.util.function.BooleanSupplier;
import java.util.function.Function;

/**
 * Fakes are the test doubles the routing tests run the real proxy classes against:
 * the velocity-api interfaces they touch (ProxyServer, Player, RegisteredServer, the
 * slf4j Logger) built with {@link Proxy}, and a stub felis-api on a loopback port.
 * They behave the way the real thing does where the code under test relies on it:
 * server names are case-insensitive, registering a taken name at another address
 * throws, and unregistering needs the exact ServerInfo that was registered.
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
     * fake builds an instance of an interface. Methods the answers leave unanswered run
     * the interface's default body when it has one and return a zero value otherwise
     * (Optional.empty() for an Optional).
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
            if (r != UNANSWERED) {
                return r;
            }
            if (m.isDefault()) {
                return InvocationHandler.invokeDefault(proxy, m, args);
            }
            return zero(m.getReturnType());
        };
        return type.cast(Proxy.newProxyInstance(type.getClassLoader(), new Class<?>[]{type}, h));
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
        if (t == Optional.class) {
            return Optional.empty();
        }
        return null;
    }

    /** text is the plain text of a component the code under test built with Component.text. */
    static String text(Component c) {
        StringBuilder sb = new StringBuilder();
        if (c instanceof TextComponent) {
            sb.append(((TextComponent) c).content());
        }
        for (Component child : c.children()) {
            sb.append(text(child));
        }
        return sb.toString();
    }

    /** await polls until the condition holds, failing after two seconds (async work). */
    static void await(String what, BooleanSupplier condition) {
        long end = System.nanoTime() + 2_000_000_000L;
        while (!condition.getAsBoolean()) {
            if (System.nanoTime() > end) {
                throw new AssertionError("timed out waiting for: " + what);
            }
            try {
                Thread.sleep(5);
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
                throw new AssertionError("interrupted waiting for: " + what);
            }
        }
    }

    // ---- the proxy ----

    static final class Net {
        private final Map<String, RegisteredServer> servers = new ConcurrentHashMap<>();
        final Map<UUID, Player> players = new ConcurrentHashMap<>();
        /** registrations logs every register ("+name host:port") and unregister ("-name"). */
        final List<String> registrations = Collections.synchronizedList(new ArrayList<>());
        /** channels lists every plugin-message channel id registered. */
        final List<String> channels = Collections.synchronizedList(new ArrayList<>());

        private final ChannelRegistrar registrar = fake(ChannelRegistrar.class, (m, a) -> {
            if ("register".equals(m)) {
                for (Object id : (Object[]) a[0]) {
                    channels.add(((ChannelIdentifier) id).getId());
                }
                return null;
            }
            return UNANSWERED;
        });

        final ProxyServer proxy = fake(ProxyServer.class, (m, a) -> {
            switch (m) {
                case "getServer":
                    return Optional.ofNullable(servers.get(key((String) a[0])));
                case "registerServer":
                    return register((ServerInfo) a[0]);
                case "unregisterServer":
                    unregister((ServerInfo) a[0]);
                    return null;
                case "getPlayer":
                    return a[0] instanceof UUID
                            ? Optional.ofNullable(players.get((UUID) a[0]))
                            : Optional.empty();
                case "getAllServers":
                    return new ArrayList<>(servers.values());
                case "getChannelRegistrar":
                    return registrar;
                default:
                    return UNANSWERED;
            }
        });

        private static String key(String name) {
            return name.toLowerCase(Locale.ROOT);
        }

        private RegisteredServer register(ServerInfo info) {
            RegisteredServer existing = servers.get(key(info.getName()));
            if (existing != null) {
                if (existing.getServerInfo().equals(info)) {
                    return existing;
                }
                throw new IllegalArgumentException("server " + info.getName() + " is already registered");
            }
            RegisteredServer rs = server(info);
            servers.put(key(info.getName()), rs);
            InetSocketAddress addr = info.getAddress();
            registrations.add("+" + info.getName() + " " + addr.getHostString() + ":" + addr.getPort());
            return rs;
        }

        private void unregister(ServerInfo info) {
            RegisteredServer existing = servers.get(key(info.getName()));
            if (existing == null || !existing.getServerInfo().equals(info)) {
                throw new IllegalArgumentException("server " + info.getName() + " is not registered with that info");
            }
            servers.remove(key(info.getName()));
            registrations.add("-" + info.getName());
        }

        /** add registers a server the way velocity.toml would (not logged in registrations). */
        RegisteredServer add(String name, String host, int port) {
            RegisteredServer rs = server(new ServerInfo(name, InetSocketAddress.createUnresolved(host, port)));
            servers.put(key(name), rs);
            return rs;
        }

        void remove(String name) {
            servers.remove(key(name));
        }

        /** address is where a registered server points, or null. */
        String address(String name) {
            RegisteredServer rs = servers.get(key(name));
            if (rs == null) {
                return null;
            }
            InetSocketAddress a = rs.getServerInfo().getAddress();
            return a.getHostString() + ":" + a.getPort();
        }
    }

    static RegisteredServer server(ServerInfo info) {
        return fake(RegisteredServer.class, (m, a) -> "getServerInfo".equals(m) ? info : UNANSWERED);
    }

    // ---- players ----

    static final class FakePlayer {
        final UUID id;
        final String name;
        volatile Locale locale = Locale.ENGLISH;
        volatile String virtualHost;
        volatile RegisteredServer current;
        volatile boolean connectSucceeds = true;
        volatile String disconnectedWith;
        final List<String> messages = Collections.synchronizedList(new ArrayList<>());
        /**
         * refusals maps a backend to the kick it answers this player's login with (a ban,
         * the whitelist, "server is full"): the connect fails with that reason.
         */
        final Map<String, String> refusals = new ConcurrentHashMap<>();
        /**
         * preConnect stands in for the proxy's ServerPreConnectEvent: it maps the server a
         * request asked for to the one it goes to, or null when a listener denied it.
         */
        volatile Function<RegisteredServer, RegisteredServer> preConnect = Function.identity();
        /** connects lists every server a connection went to (after pre-connect), by name. */
        final List<String> connects = Collections.synchronizedList(new ArrayList<>());
        /** pluginMessages lists every plugin message sent down any of this player's server connections. */
        final List<byte[]> pluginMessages = Collections.synchronizedList(new ArrayList<>());

        final Player player;

        FakePlayer(Net net, String name, int n) {
            this.id = UUID.fromString(String.format("00000000-0000-0000-0000-%012d", n));
            this.name = name;
            PlayerSettings settings = fake(PlayerSettings.class,
                    (m, a) -> "getLocale".equals(m) ? locale : UNANSWERED);
            this.player = fake(Player.class, (m, a) -> {
                switch (m) {
                    case "getUniqueId":
                        return id;
                    case "getUsername":
                        return name;
                    case "getPlayerSettings":
                        return settings;
                    case "sendMessage":
                        for (Object o : a) {
                            if (o instanceof Component) {
                                messages.add(text((Component) o));
                            }
                        }
                        return null;
                    case "disconnect":
                        disconnectedWith = text((Component) a[0]);
                        return null;
                    case "getVirtualHost":
                        String host = virtualHost;
                        return host == null
                                ? Optional.empty()
                                : Optional.of(InetSocketAddress.createUnresolved(host, 25565));
                    case "getCurrentServer":
                        RegisteredServer on = current;
                        return on == null ? Optional.empty() : Optional.of(on(on));
                    case "createConnectionRequest":
                        return request((RegisteredServer) a[0]);
                    default:
                        return UNANSWERED;
                }
            });
            net.players.put(id, player);
        }

        /** on is this player's connection to a backend, as a plugin message arrives on it. */
        ServerConnection on(RegisteredServer on) {
            return fake(ServerConnection.class, (m, a) -> {
                switch (m) {
                    case "getServer":
                        return on;
                    case "getServerInfo":
                        return on.getServerInfo();
                    case "getPlayer":
                        return player;
                    case "sendPluginMessage":
                        pluginMessages.add((byte[]) a[1]);
                        return true;
                    default:
                        return UNANSWERED;
                }
            });
        }

        private ConnectionRequestBuilder request(RegisteredServer target) {
            return fake(ConnectionRequestBuilder.class, (m, a) -> {
                switch (m) {
                    case "getServer":
                        return target;
                    case "connect":
                        RegisteredServer dest = preConnect.apply(target);
                        RegisteredServer attempted = dest == null ? target : dest;
                        String refusal = dest == null ? null : refusals.get(dest.getServerInfo().getName());
                        ConnectionRequestBuilder.Status status;
                        if (dest == null) {
                            status = ConnectionRequestBuilder.Status.CONNECTION_CANCELLED;
                        } else {
                            connects.add(dest.getServerInfo().getName());
                            status = connectSucceeds && refusal == null
                                    ? ConnectionRequestBuilder.Status.SUCCESS
                                    : ConnectionRequestBuilder.Status.SERVER_DISCONNECTED;
                        }
                        return CompletableFuture.completedFuture(fake(ConnectionRequestBuilder.Result.class,
                                (m2, a2) -> {
                                    switch (m2) {
                                        case "getStatus":
                                            return status;
                                        case "getAttemptedConnection":
                                            return attempted;
                                        case "getReasonComponent":
                                            return refusal == null
                                                    ? Optional.empty()
                                                    : Optional.of(Component.text(refusal));
                                        default:
                                            return UNANSWERED;
                                    }
                                }));
                    default:
                        return UNANSWERED;
                }
            });
        }

        /** said reports whether any chat line so far contains the text. */
        boolean said(String part) {
            synchronized (messages) {
                for (String m : messages) {
                    if (m.contains(part)) {
                        return true;
                    }
                }
            }
            return false;
        }
    }

    // ---- logging ----

    static final class Log {
        final List<String> lines = Collections.synchronizedList(new ArrayList<>());

        final Logger logger = fake(Logger.class, (m, a) -> {
            switch (m) {
                case "trace":
                case "debug":
                case "info":
                case "warn":
                case "error":
                    if (a.length > 0 && a[0] instanceof String) {
                        lines.add(m.toUpperCase(Locale.ROOT) + " " + format((String) a[0], a));
                    }
                    return null;
                case "isTraceEnabled":
                case "isDebugEnabled":
                case "isInfoEnabled":
                case "isWarnEnabled":
                case "isErrorEnabled":
                    return true;
                default:
                    return UNANSWERED;
            }
        });

        // format fills slf4j {} placeholders from the rest of the call's arguments
        // (the varargs form arrives as one Object[]).
        private static String format(String pattern, Object[] a) {
            List<Object> args = new ArrayList<>();
            for (int i = 1; i < a.length; i++) {
                if (a[i] instanceof Object[]) {
                    Collections.addAll(args, (Object[]) a[i]);
                } else {
                    args.add(a[i]);
                }
            }
            StringBuilder sb = new StringBuilder();
            int from = 0;
            int next = 0;
            int at;
            while ((at = pattern.indexOf("{}", from)) >= 0) {
                sb.append(pattern, from, at).append(next < args.size() ? args.get(next++) : "{}");
                from = at + 2;
            }
            return sb.append(pattern.substring(from)).toString();
        }

        /** count is how many lines at this level contain the text. */
        int count(String level, String part) {
            int n = 0;
            synchronized (lines) {
                for (String l : lines) {
                    if (l.startsWith(level + " ") && l.contains(part)) {
                        n++;
                    }
                }
            }
            return n;
        }
    }

    // ---- felis-api ----

    /**
     * Api is a stub felis-api internal face: link status from {@link #linked}, server
     * status from {@link #ready}, wake answering 202 unless {@link #wakeError} holds an
     * answer for the server, and join-events recorded. {@link #linkDown} makes the
     * link-status route answer 500.
     *
     * <p>Status replies carry the endpoint the way the operator writes it: a server that
     * is up reports {@code direct} and its {@link #address}; one that is not reports
     * {@code fallback} with the fallback server's NAME, "login", in the address field.
     */
    static final class Api implements AutoCloseable {
        final Set<UUID> linked = ConcurrentHashMap.newKeySet();
        volatile boolean linkDown;
        final Map<String, Boolean> ready = new ConcurrentHashMap<>();
        /** address is the direct endpoint a server reports while it is ready. */
        final Map<String, String> address = new ConcurrentHashMap<>();
        /** wakeError maps a server to "status code" (e.g. "403 forbidden"). */
        final Map<String, String> wakeError = new ConcurrentHashMap<>();
        volatile int joinStatus = 204;
        /** claimable names the servers the menu route reports as claimable. */
        final Set<String> claimable = ConcurrentHashMap.newKeySet();
        /** menuError and claimError map a server to "status code", like wakeError. */
        final Map<String, String> menuError = new ConcurrentHashMap<>();
        final Map<String, String> claimError = new ConcurrentHashMap<>();
        /** bodies holds the last request body per "METHOD path". */
        final Map<String, String> bodies = new ConcurrentHashMap<>();
        /** calls lists every request as "METHOD path", in arrival order. */
        final List<String> calls = Collections.synchronizedList(new ArrayList<>());

        private final HttpServer http;

        Api() throws IOException {
            http = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
            http.createContext("/", this::handle);
            http.start();
        }

        String url() {
            return "http://127.0.0.1:" + http.getAddress().getPort();
        }

        private void handle(HttpExchange ex) throws IOException {
            String method = ex.getRequestMethod();
            String path = ex.getRequestURI().getPath();
            bodies.put(method + " " + path, new String(ex.getRequestBody().readAllBytes(), StandardCharsets.UTF_8));
            calls.add(method + " " + path);
            String status = "/api/v1/internal/account/link/status/";
            String servers = "/api/v1/internal/servers/";
            if (path.startsWith(status)) {
                if (linkDown) {
                    reply(ex, 500, "{\"error\":{\"code\":\"internal\",\"message\":\"down\"}}");
                    return;
                }
                boolean yes = linked.contains(UUID.fromString(path.substring(status.length())));
                reply(ex, 200, "{\"linked\":" + yes + "}");
                return;
            }
            if (path.startsWith(servers)) {
                String[] parts = path.substring(servers.length()).split("/");
                String name = parts[0];
                String action = parts.length > 1 ? parts[1] : "";
                switch (method + " " + action) {
                    case "GET status":
                        reply(ex, 200, status(name, ready.getOrDefault(name, false)));
                        return;
                    case "POST wake":
                        if (!error(ex, wakeError.get(name))) {
                            // The real wake reply is this subset of the view: no endpoint.
                            reply(ex, 202, "{\"name\":\"" + name + "\",\"desiredState\":\"Running\","
                                    + "\"phase\":\"Stopped\",\"ready\":false}");
                        }
                        return;
                    case "GET menu":
                        if (!error(ex, menuError.get(name))) {
                            reply(ex, 200, "{\"name\":\"" + name + "\",\"phase\":\"Stopped\",\"ready\":false,"
                                    + "\"playersOnline\":3,\"playersMax\":20,\"claimable\":" + claimable.contains(name) + "}");
                        }
                        return;
                    case "POST claim":
                        if (!error(ex, claimError.get(name))) {
                            reply(ex, 200, "{\"claimed\":true}");
                        }
                        return;
                    case "POST join-event":
                        reply(ex, joinStatus, joinStatus == 204 ? "" : "{\"error\":{\"code\":\"internal\"}}");
                        return;
                    default:
                        break;
                }
            }
            reply(ex, 404, "{\"error\":{\"code\":\"not_found\",\"message\":\"stub\"}}");
        }

        // error answers with a {"error":{code,message}} envelope when one is configured.
        private static boolean error(HttpExchange ex, String statusAndCode) throws IOException {
            if (statusAndCode == null) {
                return false;
            }
            String[] sc = statusAndCode.split(" ", 2);
            reply(ex, Integer.parseInt(sc[0]), "{\"error\":{\"code\":\"" + sc[1] + "\",\"message\":\"stub " + sc[1] + "\"}}");
            return true;
        }

        private String status(String name, boolean up) {
            String addr = address.get(name);
            String endpoint = up
                    ? "\"endpointMode\":\"direct\"" + (addr == null ? "" : ",\"endpointAddress\":\"" + addr + "\"")
                    : "\"endpointMode\":\"fallback\",\"endpointAddress\":\"login\"";
            return "{\"name\":\"" + name + "\",\"subdomain\":\"" + name + "\",\"phase\":\""
                    + (up ? "Running" : "Stopped") + "\",\"ready\":" + up + "," + endpoint + "}";
        }

        private static void reply(HttpExchange ex, int status, String body) throws IOException {
            byte[] b = body.getBytes(StandardCharsets.UTF_8);
            if (b.length == 0) {
                ex.sendResponseHeaders(status, -1);
                ex.close();
                return;
            }
            ex.getResponseHeaders().set("Content-Type", "application/json");
            ex.sendResponseHeaders(status, b.length);
            try (OutputStream os = ex.getResponseBody()) {
                os.write(b);
            }
        }

        /** count is how many requests so far match "METHOD path". */
        int count(String call) {
            int n = 0;
            synchronized (calls) {
                for (String c : calls) {
                    if (c.equals(call)) {
                        n++;
                    }
                }
            }
            return n;
        }

        @Override
        public void close() {
            http.stop(0);
        }
    }
}
