package best.lolicon.felis.velocity;

import best.lolicon.felis.link.LinkConfig;
import best.lolicon.felis.link.LinkConfigLoader;

import java.io.IOException;
import java.io.InputStream;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.Locale;
import java.util.Properties;

/**
 * FelisVelocityConfig extends the shared link config with the two inputs only the
 * full proxy needs: the {@code root-domain} the deployment serves (the zone
 * subdomains are carved from) and the {@code lobby-server} waiters are parked in
 * while their backend wakes. It reuses {@link LinkConfigLoader} for the API base
 * URL + service token (and its first-run template), so {@code /link} keeps working
 * exactly as before; these extra keys are read from the same properties file (or
 * {@code FELIS_ROOT_DOMAIN} / {@code FELIS_LOBBY_SERVER}).
 *
 * <p>Both extras are optional at load time and the routing layer degrades rather
 * than crashing: a missing {@code root-domain} disables routing (with a clear log
 * line) while {@code /link} still runs, and a missing {@code lobby-server} means
 * the proxy has nowhere to hold waiters, so it refuses the join with a "reconnect
 * shortly" message instead of dropping the player onto a not-yet-ready backend.
 * The root domain is the only place the deployment zone enters the proxy — it is
 * never compiled in (CI red line).
 */
final class FelisVelocityConfig {
    static final String ENV_ROOT_DOMAIN = "FELIS_ROOT_DOMAIN";
    static final String ENV_LOBBY = "FELIS_LOBBY_SERVER";
    private static final String KEY_ROOT_DOMAIN = "root-domain";
    private static final String KEY_LOBBY = "lobby-server";

    private final LinkConfig linkConfig;
    private final String rootDomain;  // null → routing disabled
    private final String lobbyServer; // null → no lobby to park waiters in

    private FelisVelocityConfig(LinkConfig linkConfig, String rootDomain, String lobbyServer) {
        this.linkConfig = linkConfig;
        this.rootDomain = rootDomain;
        this.lobbyServer = lobbyServer;
    }

    static FelisVelocityConfig load(Path file) throws IOException {
        LinkConfig link = LinkConfigLoader.load(file); // url + token (required) + template + validate
        Properties props = new Properties();
        if (Files.exists(file)) {
            try (InputStream in = Files.newInputStream(file)) {
                props.load(in);
            }
        }
        String root = trimToNull(firstNonBlank(System.getenv(ENV_ROOT_DOMAIN), props.getProperty(KEY_ROOT_DOMAIN)));
        String lobby = trimToNull(firstNonBlank(System.getenv(ENV_LOBBY), props.getProperty(KEY_LOBBY)));
        return new FelisVelocityConfig(link, root == null ? null : root.toLowerCase(Locale.ROOT), lobby);
    }

    LinkConfig linkConfig() {
        return linkConfig;
    }

    /** rootDomain is the deployment zone, or null when routing should stay disabled. */
    String rootDomain() {
        return rootDomain;
    }

    boolean routingEnabled() {
        return rootDomain != null;
    }

    /** lobbyServer is the velocity.toml server name waiters are parked in, or null. */
    String lobbyServer() {
        return lobbyServer;
    }

    private static String firstNonBlank(String a, String b) {
        if (a != null && !a.trim().isEmpty()) {
            return a;
        }
        return b;
    }

    private static String trimToNull(String s) {
        if (s == null) {
            return null;
        }
        String t = s.trim();
        return t.isEmpty() ? null : t;
    }
}
