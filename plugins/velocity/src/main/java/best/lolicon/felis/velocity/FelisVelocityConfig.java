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
 * FelisVelocityConfig extends the shared link config with the inputs only the full
 * proxy needs: the {@code root-domain} the deployment serves (the zone subdomains
 * are carved from), the {@code login-server} every fresh connection must pass
 * through, and the post-auth {@code lobby-server} waiters are parked in while their
 * backend wakes. It reuses {@link LinkConfigLoader} for the API base
 * URL + service token (and its first-run template), so {@code /link} keeps working
 * exactly as before; these extra keys are read from the same properties file (or
 * {@code FELIS_ROOT_DOMAIN} / {@code FELIS_LOGIN_SERVER} /
 * {@code FELIS_LOBBY_SERVER}).
 *
 * <p>The routing extras are optional at load time and the routing layer degrades rather
 * than crashing: a missing {@code root-domain} disables routing (with a clear log
 * line) while {@code /link} still runs. The two server names default to the system
 * names ({@code login}/{@code lobby}) but must remain distinct: collapsing them
 * would put the waiting area on the unauthenticated side of the gate. The root
 * domain is the only place the deployment zone enters the proxy — it is never
 * compiled in (CI red line).
 */
final class FelisVelocityConfig {
    static final String ENV_ROOT_DOMAIN = "FELIS_ROOT_DOMAIN";
    static final String ENV_LOGIN = "FELIS_LOGIN_SERVER";
    static final String ENV_LOBBY = "FELIS_LOBBY_SERVER";
    private static final String KEY_ROOT_DOMAIN = "root-domain";
    private static final String KEY_LOGIN = "login-server";
    private static final String KEY_LOBBY = "lobby-server";
    private static final String DEFAULT_LOGIN = "login";
    private static final String DEFAULT_LOBBY = "lobby";

    private final LinkConfig linkConfig;
    private final String rootDomain;  // null → routing disabled
    private final String loginServer;
    private final String lobbyServer;

    private FelisVelocityConfig(LinkConfig linkConfig, String rootDomain,
                                String loginServer, String lobbyServer) {
        this.linkConfig = linkConfig;
        this.rootDomain = rootDomain;
        this.loginServer = loginServer;
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
        String login = trimToNull(firstNonBlank(System.getenv(ENV_LOGIN), props.getProperty(KEY_LOGIN)));
        String lobby = trimToNull(firstNonBlank(System.getenv(ENV_LOBBY), props.getProperty(KEY_LOBBY)));
        login = login == null ? DEFAULT_LOGIN : login;
        lobby = lobby == null ? DEFAULT_LOBBY : lobby;
        if (login.equalsIgnoreCase(lobby)) {
            throw new IOException("login-server and lobby-server must be different");
        }
        return new FelisVelocityConfig(
                link, root == null ? null : root.toLowerCase(Locale.ROOT), login, lobby);
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

    /** loginServer is the only server a fresh connection may enter. */
    String loginServer() {
        return loginServer;
    }

    /** lobbyServer is the post-auth server name waiters are parked in. */
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
