package best.lolicon.felis.link;

import java.io.IOException;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.util.Map;
import java.util.Objects;
import java.util.UUID;

/**
 * LinkClient calls the felis-api internal face to mint an account-link code for
 * an already-authenticated Minecraft player (spec §10). It is platform-agnostic —
 * Velocity (proxy) and the Fabric/Forge/NeoForge server mods all drive the same
 * client — and depends only on the JDK, so this single source file compiles
 * straight into each plugin jar with nothing to shade.
 *
 * <p>Contract (authoritative, mirrored from {@code internal/api}):
 * <ul>
 *   <li>{@code POST {apiBaseUrl}/api/v1/internal/account/link/code}</li>
 *   <li>header {@code Authorization: Bearer <serviceToken>} (constant-time
 *       compared server-side; an empty token fails closed)</li>
 *   <li>request body {@code {"mc_uuid":"<uuid>"}}</li>
 *   <li>success: HTTP 201 with {@code {"code","expires_at","panel_url"?}}
 *       ({@code panel_url} present only when a panel hostname is configured)</li>
 *   <li>failure: the {@code {"error":{"code","message"}}} envelope</li>
 * </ul>
 *
 * <p>The UUID must come from the platform's authenticated player identity, never
 * from user input — the whole security model of the flow is that the in-game side
 * proves the UUID before a code is ever minted.
 */
public final class LinkClient {
    private static final String PATH = "/api/v1/internal/account/link/code";

    private final LinkConfig config;
    private final HttpClient http;

    public LinkClient(LinkConfig config) {
        this.config = Objects.requireNonNull(config, "config");
        this.http = HttpClient.newBuilder()
                .connectTimeout(config.timeout())
                .build();
    }

    /** requestCode mints a one-time link code for the given verified UUID. */
    public LinkCode requestCode(UUID mcUuid) throws LinkException {
        Objects.requireNonNull(mcUuid, "mcUuid");
        String body = "{\"mc_uuid\":\"" + mcUuid + "\"}";
        HttpRequest req = HttpRequest.newBuilder()
                .uri(URI.create(config.apiBaseUrl() + PATH))
                .timeout(config.timeout())
                .header("Authorization", "Bearer " + config.serviceToken())
                .header("Content-Type", "application/json")
                .header("Accept", "application/json")
                .POST(HttpRequest.BodyPublishers.ofString(body))
                .build();

        HttpResponse<String> res;
        try {
            res = http.send(req, HttpResponse.BodyHandlers.ofString());
        } catch (IOException e) {
            throw new LinkException(0, "transport_error",
                    "could not reach felis-api: " + e.getMessage(), e);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            throw new LinkException(0, "interrupted", "link request interrupted", e);
        }

        int status = res.statusCode();
        String text = res.body();
        if (status == 201) {
            return parseCode(status, text);
        }
        throw parseError(status, text);
    }

    private LinkCode parseCode(int status, String text) throws LinkException {
        Object root;
        try {
            root = Json.parse(text);
        } catch (RuntimeException e) {
            throw new LinkException(status, "bad_response",
                    "malformed success body from felis-api", e);
        }
        if (!(root instanceof Map)) {
            throw new LinkException(status, "bad_response",
                    "expected a JSON object from felis-api");
        }
        Map<?, ?> obj = (Map<?, ?>) root;
        Object code = obj.get("code");
        if (!(code instanceof String) || ((String) code).isEmpty()) {
            throw new LinkException(status, "bad_response",
                    "success body missing 'code'");
        }
        Object exp = obj.get("expires_at");
        Object panelUrl = obj.get("panel_url");
        return new LinkCode((String) code,
                exp instanceof String ? (String) exp : null,
                panelUrl instanceof String && !((String) panelUrl).isEmpty()
                        ? (String) panelUrl : null);
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
            // Non-JSON error body (proxy 502, plain text, etc.): keep the
            // HTTP-status fallback message rather than guessing.
        }
        return new LinkException(status, code, message);
    }
}
