package best.lolicon.felis.limbo;

import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;

import java.io.IOException;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.util.concurrent.atomic.AtomicBoolean;

/** The gate's {@code GET /healthz}: 503 until {@link #markReady}, then 200; Limbo has no RCON for the readiness probe. */
final class Readiness {

    static final String PATH = "/healthz";

    private final HttpServer http;
    private final AtomicBoolean ready = new AtomicBoolean(false);

    private Readiness(HttpServer http) {
        this.http = http;
    }

    /** Serves the endpoint on port (0 picks a free one); throws when the port cannot be bound. */
    static Readiness start(int port) throws IOException {
        HttpServer server = HttpServer.create(new InetSocketAddress(port), 0);
        Readiness r = new Readiness(server);
        server.createContext(PATH, r::handle);
        server.setExecutor(null);
        server.start();
        return r;
    }

    int port() {
        return http.getAddress().getPort();
    }

    /** Flips the endpoint to 200 and reports whether this call did it. */
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
