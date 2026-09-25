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
 *
 * <p>Two timeouts bound each call: {@code connectTimeout} for opening the TCP
 * connection and {@code requestTimeout} for the whole exchange once it is open. A
 * felis-api that is gone fails fast on the first; one that is slow is cut off by the
 * second, so a blocked call cannot hold a plugin thread indefinitely.
 */
public final class LinkConfig {
    private final String apiBaseUrl;
    private final String serviceToken;
    private final Duration connectTimeout;
    private final Duration requestTimeout;

    public LinkConfig(String apiBaseUrl, String serviceToken, Duration connectTimeout, Duration requestTimeout) {
        this.apiBaseUrl = stripTrailingSlash(Objects.requireNonNull(apiBaseUrl, "apiBaseUrl"));
        this.serviceToken = Objects.requireNonNull(serviceToken, "serviceToken");
        this.connectTimeout = Objects.requireNonNull(connectTimeout, "connectTimeout");
        this.requestTimeout = Objects.requireNonNull(requestTimeout, "requestTimeout");
        if (this.apiBaseUrl.isEmpty()) {
            throw new IllegalArgumentException("apiBaseUrl is empty");
        }
        if (this.serviceToken.isEmpty()) {
            throw new IllegalArgumentException("serviceToken is empty");
        }
        if (this.connectTimeout.isZero() || this.connectTimeout.isNegative()) {
            throw new IllegalArgumentException("connectTimeout must be positive");
        }
        if (this.requestTimeout.isZero() || this.requestTimeout.isNegative()) {
            throw new IllegalArgumentException("requestTimeout must be positive");
        }
    }

    public LinkConfig(String apiBaseUrl, String serviceToken) {
        this(apiBaseUrl, serviceToken, Duration.ofSeconds(10), Duration.ofSeconds(10));
    }

    public String apiBaseUrl() {
        return apiBaseUrl;
    }

    public String serviceToken() {
        return serviceToken;
    }

    public Duration connectTimeout() {
        return connectTimeout;
    }

    public Duration requestTimeout() {
        return requestTimeout;
    }

    private static String stripTrailingSlash(String u) {
        String t = u.trim();
        while (t.endsWith("/")) {
            t = t.substring(0, t.length() - 1);
        }
        return t;
    }
}
