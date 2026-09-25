package best.lolicon.felis.link;

import java.time.Instant;
import java.time.format.DateTimeParseException;
import java.util.Map;

/**
 * OpLoginView is a pending op.console staff sign-in as the in-game approver sees it
 * ({@code GET /api/v1/internal/op-login/{id}}, spec §B op-login): whose account it is,
 * the address the code went to, and when and from where the sign-in was started. The
 * approve response reuses it for the account it approved, where only the username
 * and email are filled.
 *
 * <p>{@link #fromJson(Map)} tolerates absent fields like {@link ServerView}: a
 * missing string reads as "", an unparseable time as null, so a partial body never
 * throws on the proxy's command thread.
 */
public final class OpLoginView {
    private final String requestId;
    private final String username;
    private final String email;
    private final String clientIp;
    private final String userAgent;
    private final Instant createdAt;

    public OpLoginView(String requestId, String username, String email,
                       String clientIp, String userAgent, Instant createdAt) {
        this.requestId = requestId;
        this.username = username;
        this.email = email;
        this.clientIp = clientIp;
        this.userAgent = userAgent;
        this.createdAt = createdAt;
    }

    /** fromJson builds a view from a parsed show or approve body. */
    public static OpLoginView fromJson(Map<?, ?> o) {
        Instant created = null;
        String at = str(o, "created_at");
        if (!at.isEmpty()) {
            try {
                created = Instant.parse(at);
            } catch (DateTimeParseException ignored) {
                // Leave it null; the card then omits the age line.
            }
        }
        return new OpLoginView(str(o, "request_id"), str(o, "username"), str(o, "email"),
                str(o, "client_ip"), str(o, "user_agent"), created);
    }

    public String requestId() {
        return requestId;
    }

    /** username is the Felis account the sign-in is for; the approver retypes it. */
    public String username() {
        return username;
    }

    public String email() {
        return email;
    }

    /** clientIp is where the sign-in was started; "" when the API did not record one. */
    public String clientIp() {
        return clientIp;
    }

    /** userAgent is the browser that started the sign-in; may be "". */
    public String userAgent() {
        return userAgent;
    }

    /** createdAt is when the sign-in was started, or null when absent. */
    public Instant createdAt() {
        return createdAt;
    }

    private static String str(Map<?, ?> o, String key) {
        Object v = o.get(key);
        return v instanceof String ? (String) v : "";
    }
}
