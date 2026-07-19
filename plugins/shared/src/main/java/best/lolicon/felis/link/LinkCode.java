package best.lolicon.felis.link;

import java.util.Objects;

/**
 * LinkCode is the one-time account-link code the internal face mints for a
 * verified Minecraft UUID (spec §10). {@code expiresAt} is the raw RFC 3339
 * timestamp string the server returned (or {@code null} if it was omitted); the
 * plugins surface it to the player as an opaque "valid for a few minutes" hint
 * rather than reformatting it, so the in-game side stays agnostic to the exact
 * expiry policy the server enforces.
 */
public final class LinkCode {
    private final String code;
    private final String expiresAt;
    private final String panelUrl;

    public LinkCode(String code, String expiresAt, String panelUrl) {
        this.code = Objects.requireNonNull(code, "code");
        this.expiresAt = expiresAt;
        this.panelUrl = panelUrl;
    }

    public String code() {
        return code;
    }

    /** expiresAt is the raw RFC 3339 expiry string, or null if the server omitted it. */
    public String expiresAt() {
        return expiresAt;
    }

    /**
     * panelUrl is the ready-to-open web-panel URL the server minted alongside the
     * code, or null when no panel hostname is configured server-side.
     */
    public String panelUrl() {
        return panelUrl;
    }
}
