package best.lolicon.felis.link;

import java.util.ArrayList;
import java.util.Collection;
import java.util.List;
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
    private final boolean legacyForwarding;
    private final int autoRestarts;
    private final boolean startGaveUp;

    public ServerView(String name, String subdomain, String phase, boolean ready,
                      String autostartPolicy, String desiredState, String endpointMode,
                      String endpointAddress, int playersOnline, int playersMax) {
        this(name, subdomain, phase, ready, autostartPolicy, desiredState, endpointMode,
                endpointAddress, playersOnline, playersMax, false);
    }

    public ServerView(String name, String subdomain, String phase, boolean ready,
                      String autostartPolicy, String desiredState, String endpointMode,
                      String endpointAddress, int playersOnline, int playersMax,
                      boolean legacyForwarding) {
        this(name, subdomain, phase, ready, autostartPolicy, desiredState, endpointMode,
                endpointAddress, playersOnline, playersMax, legacyForwarding, 0, false);
    }

    public ServerView(String name, String subdomain, String phase, boolean ready,
                      String autostartPolicy, String desiredState, String endpointMode,
                      String endpointAddress, int playersOnline, int playersMax,
                      boolean legacyForwarding, int autoRestarts, boolean startGaveUp) {
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
        this.legacyForwarding = legacyForwarding;
        this.autoRestarts = autoRestarts;
        this.startGaveUp = startGaveUp;
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
                intval(o, "playersMax"),
                bool(o, "legacyForwarding"),
                intval(o, "autoRestarts"),
                bool(o, "startGaveUp"));
    }

    /**
     * listFromJson reads the {@code {"servers":[...]}} envelope {@code GET /servers}
     * answers with, which is also the shape of the proxy's saved server list. Entries
     * that are not objects are skipped; text that is not such an envelope throws
     * IllegalArgumentException.
     */
    public static List<ServerView> listFromJson(String text) {
        Object root = Json.parse(text);
        if (!(root instanceof Map)) {
            throw new IllegalArgumentException("server list: not a JSON object");
        }
        return listFrom((Map<?, ?>) root);
    }

    static List<ServerView> listFrom(Map<?, ?> envelope) {
        Object arr = envelope.get("servers");
        List<ServerView> out = new ArrayList<>();
        if (arr instanceof List) {
            for (Object e : (List<?>) arr) {
                if (e instanceof Map) {
                    out.add(fromJson((Map<?, ?>) e));
                }
            }
        }
        return out;
    }

    /** listToJson renders views in the envelope {@link #listFromJson(String)} reads back. */
    public static String listToJson(Collection<ServerView> views) {
        StringBuilder b = new StringBuilder("{\"servers\":[");
        boolean first = true;
        for (ServerView v : views) {
            if (!first) {
                b.append(',');
            }
            first = false;
            b.append('{');
            field(b, "name", v.name, true);
            field(b, "subdomain", v.subdomain, false);
            field(b, "phase", v.phase, false);
            b.append(",\"ready\":").append(v.ready);
            field(b, "autostartPolicy", v.autostartPolicy, false);
            field(b, "desiredState", v.desiredState, false);
            field(b, "endpointMode", v.endpointMode, false);
            field(b, "endpointAddress", v.endpointAddress, false);
            b.append(",\"playersOnline\":").append(v.playersOnline);
            b.append(",\"playersMax\":").append(v.playersMax);
            b.append(",\"legacyForwarding\":").append(v.legacyForwarding);
            b.append(",\"autoRestarts\":").append(v.autoRestarts);
            b.append(",\"startGaveUp\":").append(v.startGaveUp);
            b.append('}');
        }
        return b.append("]}").toString();
    }

    // field appends "key":"value", or "key":null for an absent value.
    private static void field(StringBuilder b, String key, String value, boolean first) {
        if (!first) {
            b.append(',');
        }
        b.append(Json.quote(key)).append(':').append(value == null ? "null" : Json.quote(value));
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

    /**
     * legacyForwarding is true when the server's CR is labelled
     * {@code felis.lolicon.best/forwarding=legacy}: the proxy forwards its players
     * BungeeCord-style instead of modern forwarding ({@code LegacyForwarding}).
     */
    public boolean legacyForwarding() {
        return legacyForwarding;
    }

    /** autoRestarts is how often the operator recreated the pod of a start that timed out. */
    public int autoRestarts() {
        return autoRestarts;
    }

    /**
     * startGaveUp is true for a Failed server no automatic retry will bring up (the
     * restarts are spent, or its spec is invalid). A Failed server without it is in
     * its restart backoff and may still come up on its own.
     */
    public boolean startGaveUp() {
        return startGaveUp;
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
