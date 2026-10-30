package best.lolicon.felis.limbo;

/** Reads the login gate's inputs from raw env values; a missing, blank or invalid value falls back to its default. */
final class GateConfig {

    static final int DEFAULT_HEALTH_PORT = 8080;
    static final long DEFAULT_TIMEOUT_SECONDS = 600L; // matches the Bind Code TTL
    static final long MIN_TIMEOUT_SECONDS = 30L;
    static final long MAX_TIMEOUT_SECONDS = 3600L;
    static final String DEFAULT_LOBBY = "lobby";

    private GateConfig() {
    }

    /** Returns FELIS_HEALTH_PORT when it is a valid port, else 8080. */
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

    /** Returns FELIS_LOGIN_TIMEOUT_SECONDS clamped to [30, 3600], else 600. */
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

    /** Returns FELIS_PANEL_HOSTNAME, else console.&lt;root domain&gt;, else null (login flow stays off). */
    static String panelHost(String panelHostname, String rootDomain) {
        String host = trimmed(panelHostname);
        if (host != null) {
            return host;
        }
        String root = trimmed(rootDomain);
        return root != null ? "console." + root : null;
    }

    /** Returns FELIS_LOBBY_SERVER, else "lobby"; set but blank means the install has no lobby. */
    static String lobby(String raw) {
        return raw == null ? DEFAULT_LOBBY : raw.trim();
    }

    static String trimmed(String raw) {
        if (raw == null) {
            return null;
        }
        String t = raw.trim();
        return t.isEmpty() ? null : t;
    }
}
