package best.lolicon.felis.velocity;

import best.lolicon.felis.link.LinkConfig;
import best.lolicon.felis.link.LinkConfigLoader;

import java.io.IOException;
import java.io.InputStream;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.Locale;
import java.util.Properties;
import java.util.function.Consumer;

// Keep login and lobby distinct; missing root-domain disables routing without disabling account linking.
final class FelisVelocityConfig {
    static final String ENV_ROOT_DOMAIN = "FELIS_ROOT_DOMAIN";
    static final String ENV_LOGIN = "FELIS_LOGIN_SERVER";
    static final String ENV_LOBBY = "FELIS_LOBBY_SERVER";
    static final String ENV_PANEL_HOSTNAME = "FELIS_PANEL_HOSTNAME";
    static final String ENV_ADMIN_HOSTNAME = "FELIS_ADMIN_HOSTNAME";
    private static final String KEY_ROOT_DOMAIN = "root-domain";
    private static final String KEY_LOGIN = "login-server";
    private static final String KEY_LOBBY = "lobby-server";
    private static final String KEY_PANEL_HOSTNAME = "panel-hostname";
    private static final String KEY_ADMIN_HOSTNAME = "admin-hostname";
    private static final String DEFAULT_LOGIN = "login";
    private static final String DEFAULT_LOBBY = "lobby";

    private final LinkConfig linkConfig;
    private final String rootDomain;  // null → routing disabled
    private final String loginServer;
    private final String lobbyServer;
    private final String panelHostname;  // null → fall back to console.<root>
    private final String adminHostname;  // null → fall back to op.console.<root>

    private FelisVelocityConfig(LinkConfig linkConfig, String rootDomain,
                                String loginServer, String lobbyServer,
                                String panelHostname, String adminHostname) {
        this.linkConfig = linkConfig;
        this.rootDomain = rootDomain;
        this.loginServer = loginServer;
        this.lobbyServer = lobbyServer;
        this.panelHostname = panelHostname;
        this.adminHostname = adminHostname;
    }

    // load reads the file; notice hears of a service token replaced in it later.
    static FelisVelocityConfig load(Path file, Consumer<String> notice) throws IOException {
        LinkConfig link = LinkConfigLoader.load(file, notice); // url + token (required) + template + validate
        Properties props = new Properties();
        if (Files.exists(file)) {
            try (InputStream in = Files.newInputStream(file)) {
                props.load(in);
            }
        }
        String root = trimToNull(firstNonBlank(System.getenv(ENV_ROOT_DOMAIN), props.getProperty(KEY_ROOT_DOMAIN)));
        String login = trimToNull(firstNonBlank(System.getenv(ENV_LOGIN), props.getProperty(KEY_LOGIN)));
        String lobby = lobbyName(System.getenv(ENV_LOBBY), props.getProperty(KEY_LOBBY));
        String panel = trimToNull(firstNonBlank(System.getenv(ENV_PANEL_HOSTNAME), props.getProperty(KEY_PANEL_HOSTNAME)));
        String admin = trimToNull(firstNonBlank(System.getenv(ENV_ADMIN_HOSTNAME), props.getProperty(KEY_ADMIN_HOSTNAME)));
        login = login == null ? DEFAULT_LOGIN : login;
        if (login.equalsIgnoreCase(lobby)) {
            throw new IOException("login-server and lobby-server must be different");
        }
        return new FelisVelocityConfig(
                link, root == null ? null : root.toLowerCase(Locale.ROOT), login, lobby,
                panel == null ? null : panel.toLowerCase(Locale.ROOT),
                admin == null ? null : admin.toLowerCase(Locale.ROOT));
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

    /** lobbyServer is the post-auth server name waiters are parked in; empty on an install without a lobby. */
    String lobbyServer() {
        return lobbyServer;
    }

    // panelHostname is the player web-panel host, falling back to console.<root-domain>;
    // null when neither is configured.
    String panelHostname() {
        if (panelHostname != null) {
            return panelHostname;
        }
        return rootDomain == null ? null : "console." + rootDomain;
    }

    // adminHostname is the staff op.console host, falling back to op.console.<root-domain>;
    // null when neither is configured.
    String adminHostname() {
        if (adminHostname != null) {
            return adminHostname;
        }
        return rootDomain == null ? null : "op.console." + rootDomain;
    }

    // lobbyName prefers the environment; a name set but left blank means the install has no lobby, an unset one the default.
    static String lobbyName(String env, String property) {
        String raw = env != null ? env : property;
        if (raw == null) {
            return DEFAULT_LOBBY;
        }
        return raw.trim();
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
