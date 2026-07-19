package best.lolicon.felis.link;

import java.util.Map;

/**
 * ServerView is the proxy-side mirror of the felis-api lifecycle view of one
 * MinecraftServer (the {@code ServerInfo} the internal face emits for
 * {@code GET /servers} and the internal status/wake replies). It is an immutable, dependency-free value object so the
 * shared link core stays zero-dependency and source-shareable across all four
 * loaders.
 *
 * <p>The name deliberately avoids {@code ServerInfo}: Velocity already owns
 * {@code com.velocitypowered.api.proxy.server.ServerInfo} (name + address), and
 * the routing code juggles both at once. {@code ServerView} is the felis lifecycle
 * record; {@code ServerInfo} is Velocity's registration handle.
 *
 * <p>{@link #fromJson(Map)} is tolerant: the wake reply carries only a subset of
 * the fields ({@code name}, {@code desiredState}, {@code phase}, {@code ready}),
 * so every accessor degrades to a null/zero default rather than throwing when a
 * field is absent. Routing decisions are driven off {@link #ready()} and
 * {@link #phase()}, which the relevant endpoints always populate.
 */
public final class ServerView {
    private final String name;
    private final String subdomain;
    private final String phase;
    private final boolean ready;
    private final String autostartPolicy;
    private final String desiredState;
    private final String endpointMode;
    private final String endpointAddress;
    private final int playersOnline;
    private final int playersMax;

    public ServerView(String name, String subdomain, String phase, boolean ready,
                      String autostartPolicy, String desiredState, String endpointMode,
                      String endpointAddress, int playersOnline, int playersMax) {
        this.name = name;
        this.subdomain = subdomain;
        this.phase = phase;
        this.ready = ready;
        this.autostartPolicy = autostartPolicy;
        this.desiredState = desiredState;
        this.endpointMode = endpointMode;
        this.endpointAddress = endpointAddress;
        this.playersOnline = playersOnline;
        this.playersMax = playersMax;
    }

    /** fromJson builds a view from a parsed felis-api object, tolerating absent fields. */
    public static ServerView fromJson(Map<?, ?> o) {
        return new ServerView(
                str(o, "name"),
                str(o, "subdomain"),
                str(o, "phase"),
                bool(o, "ready"),
                str(o, "autostartPolicy"),
                str(o, "desiredState"),
                str(o, "endpointMode"),
                str(o, "endpointAddress"),
                intval(o, "playersOnline"),
                intval(o, "playersMax"));
    }

    public String name() {
        return name;
    }

    public String subdomain() {
        return subdomain;
    }

    public String phase() {
        return phase;
    }

    /** ready is the authoritative "RCON-confirmed up" gate the proxy routes on. */
    public boolean ready() {
        return ready;
    }

    public String autostartPolicy() {
        return autostartPolicy;
    }

    public String desiredState() {
        return desiredState;
    }

    public String endpointMode() {
        return endpointMode;
    }

    /** endpointAddress is the {@code host[:port]} the proxy registers as a backend. */
    public String endpointAddress() {
        return endpointAddress;
    }

    public int playersOnline() {
        return playersOnline;
    }

    public int playersMax() {
        return playersMax;
    }

    private static String str(Map<?, ?> o, String key) {
        Object v = o.get(key);
        return v instanceof String ? (String) v : null;
    }

    private static boolean bool(Map<?, ?> o, String key) {
        Object v = o.get(key);
        return v instanceof Boolean && (Boolean) v;
    }

    private static int intval(Map<?, ?> o, String key) {
        Object v = o.get(key);
        // Json parses every number as Double; the player counts are int32 server-side.
        return v instanceof Number ? ((Number) v).intValue() : 0;
    }
}
