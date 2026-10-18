package best.lolicon.felis.link;

import java.util.Objects;

// Keep expiresAt as the server's opaque RFC 3339 display hint.
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

    /** panelUrl is the ready-to-open web-panel URL the server minted alongside the code, or null when no panel hostname is configured. */
    public String panelUrl() {
        return panelUrl;
    }
}
