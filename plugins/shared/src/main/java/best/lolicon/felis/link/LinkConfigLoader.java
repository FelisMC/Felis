package best.lolicon.felis.link;

import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.time.Duration;
import java.util.Properties;
import java.util.function.UnaryOperator;

/**
 * LinkConfigLoader resolves a {@link LinkConfig} the same way on every platform:
 * environment variables ({@code FELIS_API_BASE_URL}, {@code FELIS_SERVICE_TOKEN})
 * win, falling back to a {@code felis-link.properties} file in the platform's
 * config directory. Neither the API base URL nor the service token is ever
 * compiled in — this loader is the single seam each loader's entrypoint calls, so
 * the source tree stays domain- and credential-free. On first run it writes a
 * commented template and then reports the values as missing, so an operator gets
 * a file to fill in rather than a silent half-configured plugin.
 *
 * <p>The two call timeouts ({@code connect-timeout-seconds},
 * {@code request-timeout-seconds}, or {@code FELIS_API_CONNECT_TIMEOUT_SECONDS} /
 * {@code FELIS_API_REQUEST_TIMEOUT_SECONDS}) are optional and default to
 * {@value #DEFAULT_TIMEOUT_SECONDS} s. A value that is not a whole number of seconds
 * from 1 to {@value #MAX_TIMEOUT_SECONDS} fails the load: a typo quietly falling back
 * to the default would hide the setting the operator meant to change.
 */
public final class LinkConfigLoader {
    public static final String ENV_URL = "FELIS_API_BASE_URL";
    public static final String ENV_TOKEN = "FELIS_SERVICE_TOKEN";
    private static final String KEY_URL = "api-base-url";
    public static final String ENV_CONNECT_TIMEOUT = "FELIS_API_CONNECT_TIMEOUT_SECONDS";
    public static final String ENV_REQUEST_TIMEOUT = "FELIS_API_REQUEST_TIMEOUT_SECONDS";
    static final int DEFAULT_TIMEOUT_SECONDS = 10;
    static final int MAX_TIMEOUT_SECONDS = 120;
    private static final String KEY_TOKEN = "service-token";
    private static final String KEY_CONNECT_TIMEOUT = "connect-timeout-seconds";
    private static final String KEY_REQUEST_TIMEOUT = "request-timeout-seconds";

    private LinkConfigLoader() {
    }

    /**
     * load resolves config for the given properties file path, writing a template
     * if the file does not yet exist.
     *
     * @throws IOException if the file cannot be read/created, or if neither the
     *     environment nor the file supplies both required values, or if a
     *     timeout is not a whole number of seconds in range.
     */
    public static LinkConfig load(Path propertiesFile) throws IOException {
        return load(propertiesFile, System::getenv);
    }

    // load with the environment passed in, so the precedence rules are testable.
    static LinkConfig load(Path propertiesFile, UnaryOperator<String> env) throws IOException {
        Properties props = new Properties();
        if (Files.exists(propertiesFile)) {
            try (InputStream in = Files.newInputStream(propertiesFile)) {
                props.load(in);
            }
        } else {
            writeTemplate(propertiesFile);
        }

        String url = firstNonBlank(env.apply(ENV_URL), props.getProperty(KEY_URL));
        String token = firstNonBlank(env.apply(ENV_TOKEN), props.getProperty(KEY_TOKEN));

        if (isBlank(url) || isBlank(token)) {
            throw new IOException("set " + ENV_URL + "/" + ENV_TOKEN
                    + " or fill in " + propertiesFile + " (" + KEY_URL + ", " + KEY_TOKEN + ")");
        }
        Duration connect = seconds(KEY_CONNECT_TIMEOUT, ENV_CONNECT_TIMEOUT,
                firstNonBlank(env.apply(ENV_CONNECT_TIMEOUT), props.getProperty(KEY_CONNECT_TIMEOUT)));
        Duration request = seconds(KEY_REQUEST_TIMEOUT, ENV_REQUEST_TIMEOUT,
                firstNonBlank(env.apply(ENV_REQUEST_TIMEOUT), props.getProperty(KEY_REQUEST_TIMEOUT)));
        return new LinkConfig(url, token, connect, request);
    }

    private static Duration seconds(String key, String envName, String raw) throws IOException {
        if (isBlank(raw)) {
            return Duration.ofSeconds(DEFAULT_TIMEOUT_SECONDS);
        }
        String v = raw.trim();
        int n;
        try {
            n = Integer.parseInt(v);
        } catch (NumberFormatException e) {
            n = 0;
        }
        if (n < 1 || n > MAX_TIMEOUT_SECONDS) {
            throw new IOException(key + " (" + envName + ") must be a whole number of seconds from 1 to "
                    + MAX_TIMEOUT_SECONDS + ", got \"" + v + "\"");
        }
        return Duration.ofSeconds(n);
    }

    private static void writeTemplate(Path file) throws IOException {
        Path parent = file.getParent();
        if (parent != null) {
            Files.createDirectories(parent);
        }
        String template =
                "# Felis link configuration.\n"
                + "# Both values are normally injected via environment variables\n"
                + "# (" + ENV_URL + ", " + ENV_TOKEN + "); this file is the fallback.\n"
                + "#\n"
                + "# " + KEY_URL + ": base URL of the felis-api internal face: the ClusterIP\n"
                + "# of Service felis-api-internal, port 8081, e.g. http://10.43.0.10:8081\n"
                + "# (kubectl -n felis get svc felis-api-internal). The installer writes it for\n"
                + "# the proxy; it is reachable from the k3s node and permitted pods only.\n"
                + KEY_URL + "=\n"
                + "#\n"
                + "# " + KEY_TOKEN + ": the internal service token (keep this secret).\n"
                + KEY_TOKEN + "=\n"
                + "#\n"
                + "# Optional call timeouts in whole seconds (1-" + MAX_TIMEOUT_SECONDS + ", default "
                + DEFAULT_TIMEOUT_SECONDS + "): " + KEY_CONNECT_TIMEOUT + " bounds opening the\n"
                + "# connection, " + KEY_REQUEST_TIMEOUT + " the whole call once it is open\n"
                + "# (" + ENV_CONNECT_TIMEOUT + ", " + ENV_REQUEST_TIMEOUT + ").\n"
                + "#" + KEY_CONNECT_TIMEOUT + "=" + DEFAULT_TIMEOUT_SECONDS + "\n"
                + "#" + KEY_REQUEST_TIMEOUT + "=" + DEFAULT_TIMEOUT_SECONDS + "\n";
        try (OutputStream out = Files.newOutputStream(file)) {
            out.write(template.getBytes(StandardCharsets.UTF_8));
        }
    }

    private static String firstNonBlank(String a, String b) {
        return !isBlank(a) ? a : b;
    }

    private static boolean isBlank(String s) {
        return s == null || s.trim().isEmpty();
    }
}
