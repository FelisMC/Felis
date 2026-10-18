package best.lolicon.felis.link;

import java.io.IOException;
import java.net.URI;
import java.net.URLEncoder;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.net.http.HttpTimeoutException;
import java.nio.charset.StandardCharsets;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.Objects;
import java.util.Set;
import java.util.UUID;
import java.util.concurrent.ThreadLocalRandom;
import java.util.regex.Pattern;

// API failures preserve HTTP status; wake uses 403 for policy, 429 for cooldown, and 503 for capacity.
// Validate every path segment before sending requests built from plugin or chat input.
public final class FelisApiClient {
    // Mirrors internal/naming.serverNameRE plus its no-leading/trailing-dash rule.
    private static final Pattern SERVER_NAME = Pattern.compile("^[a-z0-9][a-z0-9-]{1,30}[a-z0-9]$");
    // Answers a second GET can outlive: felis-api restarting behind its Service, or a
    // gateway with no ready endpoint.
    private static final Set<Integer> RETRY_STATUSES = Set.of(502, 503, 504);
    private static final int RETRY_PAUSE_MIN_MILLIS = 100;
    private static final int RETRY_PAUSE_JITTER_MILLIS = 300;

    private final LinkConfig config;
    private final HttpClient http;
    private final ApiStats stats = new ApiStats();

    public FelisApiClient(LinkConfig config) {
        this.config = Objects.requireNonNull(config, "config");
        this.http = HttpClient.newBuilder()
                .connectTimeout(config.connectTimeout())
                .build();
    }

    /** stats counts this client's calls and their outcomes, for the plugin's health line. */
    public ApiStats stats() {
        return stats;
    }

    public EntryPolicy entryPolicy() throws LinkException {
        return EntryPolicy.fromJson(getObject("/api/v1/internal/settings/entry-policy", 200));
    }

    /** listServers returns the lifecycle view of every MinecraftServer (GET /servers). */
    public List<ServerView> listServers() throws LinkException {
        return ServerView.listFrom(getObject("/api/v1/servers", 200));
    }

    /** serverStatus reads one server's current lifecycle view (internal status). */
    public ServerView serverStatus(String name) throws LinkException {
        return ServerView.fromJson(getObject("/api/v1/internal/servers/" + serverSegment(name) + "/status", 200));
    }

    // Wake returns asynchronous lifecycle state; expose structured HTTP refusals through LinkException.
    public ServerView wake(String name, UUID mcUuid) throws LinkException {
        Objects.requireNonNull(name, "name");
        Objects.requireNonNull(mcUuid, "mcUuid");
        String body = "{\"mc_uuid\":\"" + mcUuid + "\"}";
        return ServerView.fromJson(postObject("/api/v1/internal/servers/" + serverSegment(name) + "/wake", body, 202));
    }

    /** reportJoin tells felis-api a real player joined name: it bumps last_active_at against the reaper and appends the UUID to the allowlist. Expects 204. */
    public void reportJoin(String name, UUID mcUuid) throws LinkException {
        Objects.requireNonNull(name, "name");
        Objects.requireNonNull(mcUuid, "mcUuid");
        String body = "{\"mc_uuid\":\"" + mcUuid + "\"}";
        HttpResponse<String> res = send(post("/api/v1/internal/servers/" + serverSegment(name) + "/join-event", body));
        int status = res.statusCode();
        if (status != 204 && status != 200) {
            throw parseError(status, res.body());
        }
    }

    // Claim checks ownership and quota; the subsequent wake separately enforces autostart policy.
    // Use the connection's verified UUID, never client-supplied identity.
    public void claim(String name, UUID mcUuid) throws LinkException {
        Objects.requireNonNull(name, "name");
        Objects.requireNonNull(mcUuid, "mcUuid");
        String body = "{\"mc_uuid\":\"" + mcUuid + "\"}";
        Map<?, ?> res = postObject("/api/v1/internal/servers/" + serverSegment(name) + "/claim", body, 200);
        Object claimed = res.get("claimed");
        if (!(claimed instanceof Boolean) || !((Boolean) claimed)) {
            // A 200 that does not affirm the claim is a contract breach, not a refusal: every refusal already threw above. Fail loud rather than wake a server the caller does not own.
            throw new LinkException(200, "bad_response", "claim returned 200 without claimed=true");
        }
    }

    public MenuStatus menuStatus(String name) throws LinkException {
        Objects.requireNonNull(name, "name");
        return MenuStatus.fromJson(getObject("/api/v1/internal/servers/" + serverSegment(name) + "/menu", 200));
    }

    // Drop non-string entries from per-player menu access verdicts.
    public Map<String, String> menuAccess(UUID mcUuid) throws LinkException {
        Objects.requireNonNull(mcUuid, "mcUuid");
        Map<?, ?> obj = getObject("/api/v1/internal/player/menu-access/" + mcUuid, 200);
        Map<String, String> out = new HashMap<>();
        if (obj.get("servers") instanceof Map<?, ?> servers) {
            for (Map.Entry<?, ?> e : servers.entrySet()) {
                if (e.getKey() instanceof String name && e.getValue() instanceof String verdict) {
                    out.put(name, verdict);
                }
            }
        }
        return out;
    }

    // Anything but linked:true, including a missing field, reads as not yet linked: the caller keeps waiting rather than admitting on an ambiguous body.
    public boolean linkStatus(UUID mcUuid) throws LinkException {
        Objects.requireNonNull(mcUuid, "mcUuid");
        Map<?, ?> obj = getObject("/api/v1/internal/account/link/status/" + mcUuid, 200);
        Object linked = obj.get("linked");
        return linked instanceof Boolean && (Boolean) linked;
    }

    // Blacklist only explicit UUID matches; transport errors fail closed and mint/link checks remain authoritative.
    public boolean isBlacklisted(UUID mcUuid) throws LinkException {
        Objects.requireNonNull(mcUuid, "mcUuid");
        Map<?, ?> obj = getObject("/api/v1/internal/player/blacklist/" + mcUuid, 200);
        Object barred = obj.get("blacklisted");
        return barred instanceof Boolean && (Boolean) barred;
    }

    // Only linked administrators can inspect pending staff sign-ins.
    // requestId is percent-encoded here; the Velocity command validates its charset before calling.
    public OpLoginView opLoginShow(String requestId, UUID approverUuid) throws LinkException {
        Objects.requireNonNull(requestId, "requestId");
        Objects.requireNonNull(approverUuid, "approverUuid");
        return OpLoginView.fromJson(getObject("/api/v1/internal/op-login/" + segment(requestId)
                + "?approver_uuid=" + approverUuid, 200));
    }

    // A successful approval response must affirm approved:true; mismatched account names leave the request pending.
    public OpLoginView opLoginApprove(String requestId, UUID approverUuid, String username) throws LinkException {
        Objects.requireNonNull(requestId, "requestId");
        Objects.requireNonNull(approverUuid, "approverUuid");
        Objects.requireNonNull(username, "username");
        String body = "{\"approver_uuid\":\"" + approverUuid + "\",\"username\":" + Json.quote(username) + "}";
        Map<?, ?> res = postObject("/api/v1/internal/op-login/" + segment(requestId) + "/approve", body, 200);
        Object approved = res.get("approved");
        if (!(approved instanceof Boolean) || !((Boolean) approved)) {
            throw new LinkException(200, "bad_response", "approve returned 200 without approved=true");
        }
        return OpLoginView.fromJson(res);
    }

    // Begin migration with the verified in-game UUID; web step-up and code redemption supply further proof.
    // A 201 migration response must affirm started:true.
    public void migrateStart(UUID mcUuid) throws LinkException {
        Objects.requireNonNull(mcUuid, "mcUuid");
        String body = "{\"mc_uuid\":\"" + mcUuid + "\"}";
        Map<?, ?> res = postObject("/api/v1/internal/account/migrate/start", body, 201);
        Object started = res.get("started");
        if (!(started instanceof Boolean) || !((Boolean) started)) {
            throw new LinkException(201, "bad_response", "migrate start returned 201 without started=true");
        }
    }


    // Reject invalid server segments without echoing untrusted names.
    static String serverSegment(String name) throws LinkException {
        Objects.requireNonNull(name, "name");
        if (!SERVER_NAME.matcher(name).matches()) {
            throw new LinkException(400, "invalid_server_name", "not a valid Felis server name");
        }
        return name;
    }

    /** segment percent-encodes one opaque path segment; "." and ".." are refused. */
    static String segment(String value) throws LinkException {
        Objects.requireNonNull(value, "value");
        if (value.isEmpty() || value.equals(".") || value.equals("..")) {
            throw new LinkException(400, "invalid_path_segment", "not a valid request path segment");
        }
        return URLEncoder.encode(value, StandardCharsets.UTF_8).replace("+", "%20");
    }

    // Retry safe GETs once for connection failures or 502/503/504; never retry potentially committed POSTs.
    // Do not retry timed-out GETs; jitter other retries to avoid synchronized bursts.
    private Map<?, ?> getObject(String path, int expect) throws LinkException {
        HttpRequest req = base(path).GET().build();
        try {
            HttpResponse<String> res = exchange(req);
            if (!RETRY_STATUSES.contains(res.statusCode())) {
                return expectObject(res, expect);
            }
        } catch (HttpTimeoutException e) {
            throw transportError(e);
        } catch (IOException e) {
            // refused or reset: retried below
        } catch (InterruptedException e) {
            throw interrupted(e);
        }
        try {
            Thread.sleep(RETRY_PAUSE_MIN_MILLIS + ThreadLocalRandom.current().nextInt(RETRY_PAUSE_JITTER_MILLIS + 1));
        } catch (InterruptedException e) {
            throw interrupted(e);
        }
        stats.retried();
        return expectObject(send(req), expect);
    }

    private Map<?, ?> postObject(String path, String body, int expect) throws LinkException {
        return expectObject(send(post(path, body)), expect);
    }

    private HttpRequest post(String path, String body) throws LinkException {
        return base(path)
                .header("Content-Type", "application/json")
                .POST(HttpRequest.BodyPublishers.ofString(body))
                .build();
    }

    private HttpRequest.Builder base(String path) throws LinkException {
        URI uri;
        try {
            uri = URI.create(config.apiBaseUrl() + path);
        } catch (IllegalArgumentException e) {
            // Unreachable for paths built from the segment helpers; kept so a malformed base URL surfaces as a LinkException rather than an unchecked throw out of a scheduler task.
            throw new LinkException(0, "bad_request", "could not build the felis-api request URL", e);
        }
        return HttpRequest.newBuilder()
                .uri(uri)
                .timeout(config.requestTimeout())
                .header("Authorization", "Bearer " + config.serviceToken())
                .header("Accept", "application/json");
    }

    private HttpResponse<String> send(HttpRequest req) throws LinkException {
        try {
            return exchange(req);
        } catch (IOException e) {
            throw transportError(e);
        } catch (InterruptedException e) {
            throw interrupted(e);
        }
    }

    // exchange is the one place a request goes out, so every attempt is counted once.
    private HttpResponse<String> exchange(HttpRequest req) throws IOException, InterruptedException {
        long start = System.nanoTime();
        int status = 0;
        try {
            HttpResponse<String> res = http.send(req, HttpResponse.BodyHandlers.ofString());
            status = res.statusCode();
            return res;
        } finally {
            stats.record(status, (System.nanoTime() - start) / 1_000_000);
        }
    }

    // A refused connection arrives as a ConnectException with no message; the class
    // name keeps the log line from reading "could not reach felis-api: null".
    private static LinkException transportError(IOException e) {
        String why = e.getMessage() == null ? e.getClass().getSimpleName() : e.getMessage();
        return new LinkException(0, "transport_error", "could not reach felis-api: " + why, e);
    }

    private static LinkException interrupted(InterruptedException e) {
        Thread.currentThread().interrupt();
        return new LinkException(0, "interrupted", "felis-api request interrupted", e);
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
