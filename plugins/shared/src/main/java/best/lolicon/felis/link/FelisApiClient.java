package best.lolicon.felis.link;

import java.io.IOException;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.Objects;
import java.util.UUID;

/**
 * FelisApiClient is the proxy's read/drive client for the felis-api internal face
 * (spec §7, §9). Where {@link LinkClient} mints account-link codes, this client
 * drives domain-autostart routing: it lists the registrable servers, resolves a
 * connecting virtual host to its server, polls a server's lifecycle status, pulls
 * the wake lever, and reports real player joins. It shares the {@link LinkConfig}
 * (same internal base URL + service token) and the same zero-dependency JDK HTTP
 * stack, so it compiles straight into each loader jar with nothing to shade.
 *
 * <p>Every call authenticates with {@code Authorization: Bearer <serviceToken>}
 * and surfaces a non-success status as a {@link LinkException} carrying the HTTP
 * status, so the proxy can branch on it without parsing human text. The two that
 * matter for routing:
 * <ul>
 *   <li>{@code wake} → 403 means the autostartPolicy gate refused this UUID (do
 *       not enqueue the player); 429 means a wake is already cooling down
 *       ("already waking, keep waiting"), not a failure.</li>
 *   <li>{@code serverByHost} → 404 means the host maps to no server.</li>
 * </ul>
 */
public final class FelisApiClient {
    private final LinkConfig config;
    private final HttpClient http;

    public FelisApiClient(LinkConfig config) {
        this.config = Objects.requireNonNull(config, "config");
        this.http = HttpClient.newBuilder()
                .connectTimeout(config.timeout())
                .build();
    }

    /** listServers returns the lifecycle view of every MinecraftServer (GET /servers). */
    public List<ServerView> listServers() throws LinkException {
        Map<?, ?> obj = getObject("/api/v1/servers", 200);
        Object arr = obj.get("servers");
        List<ServerView> out = new ArrayList<>();
        if (arr instanceof List) {
            for (Object e : (List<?>) arr) {
                if (e instanceof Map) {
                    out.add(ServerView.fromJson((Map<?, ?>) e));
                }
            }
        }
        return out;
    }

    /**
     * serverByHost resolves {@code subdomain.<root_domain>} to its server view
     * (GET /servers/by-host/{host}). A 404 surfaces as a LinkException with
     * statusCode 404 so the caller can distinguish "unknown host" from a transport
     * fault.
     */
    public ServerView serverByHost(String host) throws LinkException {
        return ServerView.fromJson(getObject("/api/v1/servers/by-host/" + Objects.requireNonNull(host, "host"), 200));
    }

    /** serverStatus reads one server's current lifecycle view (internal status). */
    public ServerView serverStatus(String name) throws LinkException {
        return ServerView.fromJson(getObject("/api/v1/internal/servers/" + Objects.requireNonNull(name, "name") + "/status", 200));
    }

    /**
     * wake pulls the domain-autostart lever for {@code name} on behalf of the
     * joining player (spec §9.1, §14). The reply (202) carries the current phase
     * and ready flag so the caller can decide whether to wait. A 403 (policy gate)
     * or 429 (cooldown) arrives as a LinkException the caller branches on.
     */
    public ServerView wake(String name, UUID mcUuid) throws LinkException {
        Objects.requireNonNull(name, "name");
        Objects.requireNonNull(mcUuid, "mcUuid");
        String body = "{\"mc_uuid\":\"" + mcUuid + "\"}";
        return ServerView.fromJson(postObject("/api/v1/internal/servers/" + name + "/wake", body, 202));
    }

    /**
     * reportJoin tells felis-api a real player joined {@code name} (spec §7
     * /internal/.../join-event): it bumps last_active_at against the reaper and
     * auto-appends the UUID to the allowlist. Expects 204.
     */
    public void reportJoin(String name, UUID mcUuid) throws LinkException {
        Objects.requireNonNull(name, "name");
        Objects.requireNonNull(mcUuid, "mcUuid");
        String body = "{\"mc_uuid\":\"" + mcUuid + "\"}";
        HttpResponse<String> res = send(post("/api/v1/internal/servers/" + name + "/join-event", body));
        int status = res.statusCode();
        if (status != 204 && status != 200) {
            throw parseError(status, res.body());
        }
    }

    /**
     * claim takes ownership of an ownerless server on behalf of a player driving the
     * felis-paper lobby menu (spec §9.3, §12). It is the first of the menu's two
     * rules — claim asserts ownership and quota; the autostartPolicy gate is enforced
     * separately by the {@link #wake} that follows. Identity is the verified
     * online-mode UUID Velocity derived from the connection, never a client-supplied
     * value. Expects 200; the refusal cases surface as branchable LinkExceptions:
     * 412 {@code not_linked}, 403 {@code quota_exceeded}, 409 {@code already_claimed},
     * 404 unknown server.
     */
    public void claim(String name, UUID mcUuid) throws LinkException {
        Objects.requireNonNull(name, "name");
        Objects.requireNonNull(mcUuid, "mcUuid");
        String body = "{\"mc_uuid\":\"" + mcUuid + "\"}";
        Map<?, ?> res = postObject("/api/v1/internal/servers/" + name + "/claim", body, 200);
        Object claimed = res.get("claimed");
        if (!(claimed instanceof Boolean) || !((Boolean) claimed)) {
            // A 200 that doesn't affirm the claim is a contract breach, not a refusal —
            // every refusal (412/403/409/404) already threw above. Fail loud rather
            // than wake a server the caller doesn't actually own.
            throw new LinkException(200, "bad_response", "claim returned 200 without claimed=true");
        }
    }

    /**
     * menuStatus reads the lobby menu projection of one server (spec §12,
     * {@code GET …/menu}): the §11 lifecycle view plus the ownership-derived
     * {@code claimable} flag the lobby needs to choose a button. Velocity calls this
     * for a {@code StatusQuery} and forwards the result downstream as a
     * {@code StatusUpdate} frame. Expects 200; 404 means the server is unknown.
     */
    public MenuStatus menuStatus(String name) throws LinkException {
        Objects.requireNonNull(name, "name");
        return MenuStatus.fromJson(getObject("/api/v1/internal/servers/" + name + "/menu", 200));
    }

    // ---- transport ----

    private Map<?, ?> getObject(String path, int expect) throws LinkException {
        HttpRequest req = base(path).GET().build();
        return expectObject(send(req), expect);
    }

    private Map<?, ?> postObject(String path, String body, int expect) throws LinkException {
        return expectObject(send(post(path, body)), expect);
    }

    private HttpRequest post(String path, String body) {
        return base(path)
                .header("Content-Type", "application/json")
                .POST(HttpRequest.BodyPublishers.ofString(body))
                .build();
    }

    private HttpRequest.Builder base(String path) {
        return HttpRequest.newBuilder()
                .uri(URI.create(config.apiBaseUrl() + path))
                .timeout(config.timeout())
                .header("Authorization", "Bearer " + config.serviceToken())
                .header("Accept", "application/json");
    }

    private HttpResponse<String> send(HttpRequest req) throws LinkException {
        try {
            return http.send(req, HttpResponse.BodyHandlers.ofString());
        } catch (IOException e) {
            throw new LinkException(0, "transport_error",
                    "could not reach felis-api: " + e.getMessage(), e);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            throw new LinkException(0, "interrupted", "felis-api request interrupted", e);
        }
    }

    private Map<?, ?> expectObject(HttpResponse<String> res, int expect) throws LinkException {
        int status = res.statusCode();
        if (status != expect) {
            throw parseError(status, res.body());
        }
        Object root;
        try {
            root = Json.parse(res.body());
        } catch (RuntimeException e) {
            throw new LinkException(status, "bad_response", "malformed body from felis-api", e);
        }
        if (!(root instanceof Map)) {
            throw new LinkException(status, "bad_response", "expected a JSON object from felis-api");
        }
        return (Map<?, ?>) root;
    }

    // parseError mirrors LinkClient: extract the stable {"error":{"code","message"}}
    // envelope when present, else fall back to the HTTP status. A separate copy here
    // keeps the routing client independent of LinkClient's private internals.
    private LinkException parseError(int status, String text) {
        String code = "error";
        String message = "felis-api returned HTTP " + status;
        try {
            Object root = Json.parse(text);
            if (root instanceof Map) {
                Object err = ((Map<?, ?>) root).get("error");
                if (err instanceof Map) {
                    Object c = ((Map<?, ?>) err).get("code");
                    Object m = ((Map<?, ?>) err).get("message");
                    if (c instanceof String && !((String) c).isEmpty()) {
                        code = (String) c;
                    }
                    if (m instanceof String && !((String) m).isEmpty()) {
                        message = (String) m;
                    }
                }
            }
        } catch (RuntimeException ignored) {
            // Non-JSON error body (proxy 502, plain text, etc.): keep the fallback.
        }
        return new LinkException(status, code, message);
    }
}
