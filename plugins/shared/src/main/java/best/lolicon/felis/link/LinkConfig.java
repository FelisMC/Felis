package best.lolicon.felis.link;

import java.time.Duration;
import java.util.Objects;

/**
 * LinkConfig is the immutable configuration a {@link LinkClient} needs to reach
 * the felis-api internal face. Both the base URL and the service token are
 * deployment inputs — operator config or a Secret-injected environment variable —
 * and are <em>never</em> compiled in. Keeping them out of source is what lets the
 * tree stay domain- and credential-free; each platform's config loader is
 * responsible for sourcing them.
 */
public final class LinkConfig {
    private final String apiBaseUrl;
    private final String serviceToken;
    private final Duration timeout;

    public LinkConfig(String apiBaseUrl, String serviceToken, Duration timeout) {
        this.apiBaseUrl = stripTrailingSlash(Objects.requireNonNull(apiBaseUrl, "apiBaseUrl"));
        this.serviceToken = Objects.requireNonNull(serviceToken, "serviceToken");
        this.timeout = Objects.requireNonNull(timeout, "timeout");
        if (this.apiBaseUrl.isEmpty()) {
            throw new IllegalArgumentException("apiBaseUrl is empty");
        }
        if (this.serviceToken.isEmpty()) {
            throw new IllegalArgumentException("serviceToken is empty");
        }
        if (this.timeout.isZero() || this.timeout.isNegative()) {
            throw new IllegalArgumentException("timeout must be positive");
        }
    }

    public LinkConfig(String apiBaseUrl, String serviceToken) {
        this(apiBaseUrl, serviceToken, Duration.ofSeconds(10));
    }

    public String apiBaseUrl() {
        return apiBaseUrl;
    }

    public String serviceToken() {
        return serviceToken;
    }

    public Duration timeout() {
        return timeout;
    }

    private static String stripTrailingSlash(String u) {
        String t = u.trim();
        while (t.endsWith("/")) {
            t = t.substring(0, t.length() - 1);
        }
        return t;
    }
}
