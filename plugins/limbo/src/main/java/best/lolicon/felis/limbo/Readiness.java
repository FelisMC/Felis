package best.lolicon.felis.limbo;

import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;

import java.io.IOException;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.util.concurrent.atomic.AtomicBoolean;

/**
 * Readiness is the gate's {@code GET /healthz}: 503 "starting" until {@link #markReady},
 * 200 "ok" after. Limbo has no RCON, so this is the signal the MinecraftServer CRD's
 * HTTP readinessProbe follows instead of a bare TCP check that would pass the moment
 * the socket binds.
 */
final class Readiness {

    static final String PATH = "/healthz";

    private final HttpServer http;
    private final AtomicBoolean ready = new AtomicBoolean(false);

    private Readiness(HttpServer http) {
        this.http = http;
    }

    /**
     * start serves the endpoint on port (0 picks a free one). It throws when the port
     * cannot be bound; the caller then leaves the gate NotReady rather than advertising
     * an unstarted auth gate.
     */
    static Readiness start(int port) throws IOException {
        HttpServer server = HttpServer.create(new InetSocketAddress(port), 0);
        Readiness r = new Readiness(server);
        server.createContext(PATH, r::handle);
        server.setExecutor(null); // default executor: this endpoint is trivial
        server.start();
        return r;
    }

    int port() {
        return http.getAddress().getPort();
    }

    /** markReady flips the endpoint to 200 and reports whether this call did it. */
    boolean markReady() {
        return ready.compareAndSet(false, true);
    }

    void stop() {
        ready.set(false);
        http.stop(0);
    }

    private void handle(HttpExchange exchange) throws IOException {
        boolean up = ready.get();
        byte[] body = (up ? "ok" : "starting").getBytes(StandardCharsets.UTF_8);
        exchange.getResponseHeaders().set("Content-Type", "text/plain; charset=utf-8");
        exchange.sendResponseHeaders(up ? 200 : 503, body.length);
        try (OutputStream os = exchange.getResponseBody()) {
            os.write(body);
        }
    }
}
