package best.lolicon.felis.link;

import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.time.Duration;
import java.util.Properties;

/**
 * LinkConfigLoader resolves a {@link LinkConfig} the same way on every platform:
 * environment variables ({@code FELIS_API_BASE_URL}, {@code FELIS_SERVICE_TOKEN})
 * win, falling back to a {@code felis-link.properties} file in the platform's
 * config directory. Neither the API base URL nor the service token is ever
 * compiled in — this loader is the single seam each loader's entrypoint calls, so
 * the source tree stays domain- and credential-free. On first run it writes a
 * commented template and then reports the values as missing, so an operator gets
 * a file to fill in rather than a silent half-configured plugin.
 */
public final class LinkConfigLoader {
    public static final String ENV_URL = "FELIS_API_BASE_URL";
    public static final String ENV_TOKEN = "FELIS_SERVICE_TOKEN";
    private static final String KEY_URL = "api-base-url";
    private static final String KEY_TOKEN = "service-token";

    private LinkConfigLoader() {
    }

    /**
     * load resolves config for the given properties file path, writing a template
     * if the file does not yet exist.
     *
     * @throws IOException if the file cannot be read/created, or if neither the
     *     environment nor the file supplies both required values.
     */
    public static LinkConfig load(Path propertiesFile) throws IOException {
        Properties props = new Properties();
        if (Files.exists(propertiesFile)) {
            try (InputStream in = Files.newInputStream(propertiesFile)) {
                props.load(in);
            }
        } else {
            writeTemplate(propertiesFile);
        }

        String url = firstNonBlank(System.getenv(ENV_URL), props.getProperty(KEY_URL));
        String token = firstNonBlank(System.getenv(ENV_TOKEN), props.getProperty(KEY_TOKEN));

        if (isBlank(url) || isBlank(token)) {
            throw new IOException("set " + ENV_URL + "/" + ENV_TOKEN
                    + " or fill in " + propertiesFile + " (" + KEY_URL + ", " + KEY_TOKEN + ")");
        }
        return new LinkConfig(url, token, Duration.ofSeconds(10));
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
                + KEY_TOKEN + "=\n";
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
