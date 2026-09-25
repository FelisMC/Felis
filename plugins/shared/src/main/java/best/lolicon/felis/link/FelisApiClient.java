package best.lolicon.felis.link;

import java.io.IOException;
import java.net.URI;
import java.net.URLEncoder;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.net.http.HttpTimeoutException;
import java.nio.charset.StandardCharsets;
import java.util.List;
import java.util.Map;
import java.util.Objects;
import java.util.Set;
import java.util.UUID;
import java.util.concurrent.ThreadLocalRandom;
import java.util.regex.Pattern;

/**
 * FelisApiClient is the proxy's read/drive client for the felis-api internal face
 * (spec §7, §9). Where {@link LinkClient} mints account-link codes, this client
 * drives domain-autostart routing: it lists the registrable servers, polls a
 * server's lifecycle status, pulls the wake lever, and reports real player
 * joins. It shares the {@link LinkConfig}
 * (same internal base URL + service token) and the same zero-dependency JDK HTTP
 * stack, so it compiles straight into each loader jar with nothing to shade.
 *
 * <p>Every call authenticates with {@code Authorization: Bearer <serviceToken>}
 * and surfaces a non-success status as a {@link LinkException} carrying the HTTP
 * status, so the proxy can branch on it without parsing human text. The one that
 * matters most for routing: {@code wake} → 403 means the autostartPolicy gate
 * refused this UUID (do not enqueue the player); 429 means a wake is already
 * cooling down ("already waking, keep waiting"); 503 means the cluster is at
 * capacity (tell the player to try later — nothing is coming up).
 *
 * <p><b>Path safety.</b> Server names reach this client from plugin messages and
 * chat, so every value spliced into a request path goes through
 * {@link #serverSegment} or {@link #segment}. A name outside the platform's own
 * alphabet ({@code ^[a-z0-9-]{3,32}$}, no leading or trailing dash — the rule
 * {@code naming.ValidateServerName} enforces server-side) is refused before any
 * request is built, and what passes is percent-encoded as well. Without this a
 * WakeRequest for {@code victim/join-event?} became {@code POST
 * …/servers/victim/join-event}, which appends the sender to another tenant's
 * allowlist.
 */
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

    /** listServers returns the lifecycle view of every MinecraftServer (GET /servers). */
    public List<ServerView> listServers() throws LinkException {
        return ServerView.listFrom(getObject("/api/v1/servers", 200));
    }

    /** serverStatus reads one server's current lifecycle view (internal status). */
    public ServerView serverStatus(String name) throws LinkException {
        return ServerView.fromJson(getObject("/api/v1/internal/servers/" + serverSegment(name) + "/status", 200));
    }

    /**
     * wake pulls the domain-autostart lever for {@code name} on behalf of the
     * joining player (spec §9.1, §14). The reply (202) carries the current phase
     * and ready flag so the caller can decide whether to wait. A 403 (policy gate),
     * 409 {@code maintenance_in_progress} (a restore, backup or file write holds the
     * world), 429 (cooldown), or 503 {@code at_capacity} (running cap) arrives as a
     * LinkException the caller branches on.
     */
    public ServerView wake(String name, UUID mcUuid) throws LinkException {
        Objects.requireNonNull(name, "name");
        Objects.requireNonNull(mcUuid, "mcUuid");
        String body = "{\"mc_uuid\":\"" + mcUuid + "\"}";
        return ServerView.fromJson(postObject("/api/v1/internal/servers/" + serverSegment(name) + "/wake", body, 202));
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
        HttpResponse<String> res = send(post("/api/v1/internal/servers/" + serverSegment(name) + "/join-event", body));
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
        Map<?, ?> res = postObject("/api/v1/internal/servers/" + serverSegment(name) + "/claim", body, 200);
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
        return MenuStatus.fromJson(getObject("/api/v1/internal/servers/" + serverSegment(name) + "/menu", 200));
    }

    /**
     * linkStatus polls whether the verified UUID has finished web account-link — the
     * completion leg of the in-game login flow (spec §B3). After the player redeems
     * the Bind Code on {@code console.<root_domain>} the login limbo polls this until
     * it flips true, then admits/transfers the player. {@code GET
     * /api/v1/internal/account/link/status/{mc_uuid}} → {@code {"linked":bool}};
     * read-only and keyed by the verified UUID, so it consumes nothing and is safe to
     * poll repeatedly. Anything but {@code linked:true} (including a missing field) is
     * reported as not-yet-linked — the caller keeps waiting rather than admitting on
     * an ambiguous body.
     */
    public boolean linkStatus(UUID mcUuid) throws LinkException {
        Objects.requireNonNull(mcUuid, "mcUuid");
        Map<?, ?> obj = getObject("/api/v1/internal/account/link/status/" + mcUuid, 200);
        Object linked = obj.get("linked");
        return linked instanceof Boolean && (Boolean) linked;
    }

    /**
     * isBlacklisted reports whether a connecting UUID was barred by a
     * username-collision reclaim (spec §B3): the login limbo checks this on join and
     * refuses a barred squatter UUID before minting a code. {@code GET
     * /api/v1/internal/player/blacklist/{mc_uuid}} → {@code {"blacklisted":bool}}. The
     * bar is keyed by UUID, so the genuine Mojang player (same name, different UUID)
     * always reads false and passes. A body that does not affirm {@code
     * blacklisted:true} is treated as not-barred (fail-open on this specific read is
     * deliberate: the authoritative gate is the mint/link flow, and a transport fault
     * already surfaces as a LinkException the caller fails closed on).
     */
    public boolean isBlacklisted(UUID mcUuid) throws LinkException {
        Objects.requireNonNull(mcUuid, "mcUuid");
        Map<?, ?> obj = getObject("/api/v1/internal/player/blacklist/" + mcUuid, 200);
        Object barred = obj.get("blacklisted");
        return barred instanceof Boolean && (Boolean) barred;
    }

    /**
     * opLoginShow reads whose op.console staff sign-in a pending request is — the first
     * half of Velocity's {@code /felis web op approve <code>}, shown to the admin before
     * they vouch. {@code GET /api/v1/internal/op-login/{id}?approver_uuid=<uuid>}:
     * felis-api refuses unless the approver's verified UUID is linked to an admin or
     * owner (403 {@code not_admin}), so a player who types the command learns nothing
     * about a staff account. An id naming no live pending request is 404 {@code
     * op_login_not_found}. Both arrive as branchable {@link LinkException}s.
     *
     * <p>{@code requestId} is interpolated into the request path. It is
     * percent-encoded here, and the Velocity command also validates its charset
     * before calling.
     */
    public OpLoginView opLoginShow(String requestId, UUID approverUuid) throws LinkException {
        Objects.requireNonNull(requestId, "requestId");
        Objects.requireNonNull(approverUuid, "approverUuid");
        return OpLoginView.fromJson(getObject("/api/v1/internal/op-login/" + segment(requestId)
                + "?approver_uuid=" + approverUuid, 200));
    }

    /**
     * opLoginApprove records an in-game administrator's vouch for a pending op.console
     * staff login — the second factor of the spec §B op-login door, supplied from
     * Velocity's {@code /felis web op approve <code> <username>}. It POSTs the approver's
     * verified online-mode UUID and the account name they typed after reading
     * {@link #opLoginShow} to {@code POST /api/v1/internal/op-login/{id}/approve}.
     * felis-api refuses unless the UUID is linked to an admin or owner (403 {@code
     * not_admin}) and unless the name is the request's account (409 {@code
     * op_login_mismatch}, request left pending). An id naming no live pending request is
     * 404 {@code op_login_not_found}. All arrive as branchable {@link LinkException}s; a
     * 200 that does not affirm {@code approved:true} is a contract breach, not a
     * refusal. Returns the approved account's username and email.
     */
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

    /**
     * migrateStart opens an account migration for the player who ran {@code /felis
     * migrate} in-game (spec §B3 inherit, scenario A). It is the internal face of that
     * command: Velocity has already established the caller's Mojang-verified UUID, so the
     * initiator is trustworthy, and this POST puts that UUID's linked account into migrate
     * mode (state {@code initiated}). Only the migration is opened here — the sensitive
     * proof (web step-up, naming the receiving account, redeeming a code) happens
     * afterwards on the console. {@code POST /api/v1/internal/account/migrate/start} with
     * the verified UUID; expects 201. An unlinked UUID has no account to migrate (404
     * {@code not_linked}); a retired or already-migrating account cannot re-initiate (409
     * {@code account_retired}). Both surface as branchable {@link LinkException}s; a 201
     * that does not affirm {@code started:true} is a contract breach, not a refusal.
     */
    public void migrateStart(UUID mcUuid) throws LinkException {
        Objects.requireNonNull(mcUuid, "mcUuid");
        String body = "{\"mc_uuid\":\"" + mcUuid + "\"}";
        Map<?, ?> res = postObject("/api/v1/internal/account/migrate/start", body, 201);
        Object started = res.get("started");
        if (!(started instanceof Boolean) || !((Boolean) started)) {
            throw new LinkException(201, "bad_response", "migrate start returned 201 without started=true");
        }
    }

    // ---- transport ----

    /**
     * serverSegment admits {@code name} into a request path only when it is a
     * well-formed Felis server name. The refusal carries a 400 so callers that relay
     * {@code LinkException} messages to a player treat it as a request error, and it
     * does not echo the input back.
     */
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

    // getObject tries a GET a second time, after a jittered 100-400 ms pause, when the
    // first attempt failed in a way a retry can fix: the connection was refused or
    // reset (felis-api restarting, its pod moving) or the answer was 502/503/504. GETs
    // only read, so repeating one is safe; POSTs (wake, claim, join-event, approvals)
    // are never repeated, because the first attempt may have landed. A GET that timed
    // out is not repeated either: felis-api is then slow rather than gone, and a second
    // full wait would hold the caller twice as long while adding load to an API that
    // is already behind. The jitter keeps a proxy's queued callers from retrying in
    // one burst.
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
            // Unreachable for paths built from the segment helpers above; kept so a
            // malformed base URL surfaces as a LinkException the callers already
            // handle rather than an unchecked throw out of a scheduler task.
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
