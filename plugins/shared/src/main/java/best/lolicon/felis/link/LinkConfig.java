package best.lolicon.felis.link;

import java.time.Duration;
import java.util.Objects;
import java.util.function.Supplier;

// Deployment supplies credentials and endpoint; separate connect/request deadlines bound blocking calls.
public final class LinkConfig {
    private final String apiBaseUrl;
    private final Supplier<String> serviceToken;
    private final Duration connectTimeout;
    private final Duration requestTimeout;

    public LinkConfig(String apiBaseUrl, String serviceToken, Duration connectTimeout, Duration requestTimeout) {
        this(apiBaseUrl, fixed(serviceToken), connectTimeout, requestTimeout);
    }

    // LinkConfig with a token that may change while the process runs (FileToken);
    // it must hold one from the start.
    LinkConfig(String apiBaseUrl, Supplier<String> serviceToken, Duration connectTimeout, Duration requestTimeout) {
        this.apiBaseUrl = stripTrailingSlash(Objects.requireNonNull(apiBaseUrl, "apiBaseUrl"));
        this.serviceToken = Objects.requireNonNull(serviceToken, "serviceToken");
        this.connectTimeout = Objects.requireNonNull(connectTimeout, "connectTimeout");
        this.requestTimeout = Objects.requireNonNull(requestTimeout, "requestTimeout");
        if (this.apiBaseUrl.isEmpty()) {
            throw new IllegalArgumentException("apiBaseUrl is empty");
        }
        if (this.serviceToken.get().isEmpty()) {
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

    /** serviceToken is the token to present on this call; FileToken may have replaced it since the last. */
    public String serviceToken() {
        return serviceToken.get();
    }

    public Duration connectTimeout() {
        return connectTimeout;
    }

    public Duration requestTimeout() {
        return requestTimeout;
    }

    private static Supplier<String> fixed(String token) {
        Objects.requireNonNull(token, "serviceToken");
        return () -> token;
    }

    private static String stripTrailingSlash(String u) {
        String t = u.trim();
        while (t.endsWith("/")) {
            t = t.substring(0, t.length() - 1);
        }
        return t;
    }
}
