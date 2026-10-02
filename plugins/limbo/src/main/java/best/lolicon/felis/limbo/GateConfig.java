package best.lolicon.felis.limbo;

/**
 * GateConfig reads the login gate's deployment inputs from their raw environment
 * values. A value that is missing, blank or out of range falls back to its default
 * (or its bound), so a typo in the image env never keeps the gate from starting.
 */
final class GateConfig {

    static final int DEFAULT_HEALTH_PORT = 8080;
    // The default window (10 min) matches the Bind Code TTL (LinkCodeTTL in
    // internal/api): no point holding a player past code expiry, and no point cutting
    // them off while it is still valid.
    static final long DEFAULT_TIMEOUT_SECONDS = 600L;
    static final long MIN_TIMEOUT_SECONDS = 30L;
    static final long MAX_TIMEOUT_SECONDS = 3600L;
    static final String DEFAULT_LOBBY = "lobby";

    private GateConfig() {
    }

    /** healthPort is FELIS_HEALTH_PORT when it is a port number, else 8080. */
    static int healthPort(String raw) {
        String t = trimmed(raw);
        if (t != null) {
            try {
                int p = Integer.parseInt(t);
                if (p > 0 && p < 65536) {
                    return p;
                }
            } catch (NumberFormatException ignored) {
                // fall through to the default
            }
        }
        return DEFAULT_HEALTH_PORT;
    }

    /** loginTimeoutSeconds is FELIS_LOGIN_TIMEOUT_SECONDS held to [30, 3600], else 600. */
    static long loginTimeoutSeconds(String raw) {
        String t = trimmed(raw);
        if (t != null) {
            try {
                return Math.max(MIN_TIMEOUT_SECONDS, Math.min(MAX_TIMEOUT_SECONDS, Long.parseLong(t)));
            } catch (NumberFormatException ignored) {
                // fall through to the default
            }
        }
        return DEFAULT_TIMEOUT_SECONDS;
    }

    /**
     * panelHost is the console host the player links at: the resolved
     * FELIS_PANEL_HOSTNAME the provisioner bakes in (it honours a custom
     * panel_hostname), else console.&lt;FELIS_ROOT_DOMAIN&gt; for an older operator whose
     * env predates it, else null, and the login flow stays off.
     */
    static String panelHost(String panelHostname, String rootDomain) {
        String host = trimmed(panelHostname);
        if (host != null) {
            return host;
        }
        String root = trimmed(rootDomain);
        return root != null ? "console." + root : null;
    }

    /** lobby is FELIS_LOBBY_SERVER, else "lobby". */
    static String lobby(String raw) {
        String t = trimmed(raw);
        return t != null ? t : DEFAULT_LOBBY;
    }

    static String trimmed(String raw) {
        if (raw == null) {
            return null;
        }
        String t = raw.trim();
        return t.isEmpty() ? null : t;
    }
}
