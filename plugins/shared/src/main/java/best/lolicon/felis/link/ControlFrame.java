package best.lolicon.felis.link;

import java.util.ArrayList;
import java.util.Collections;
import java.util.List;
import java.util.Objects;

// Authenticate the source connection, never the frame identity; undefined fields use safe defaults.
public final class ControlFrame {

    /** Upstream: park-and-wake a server the player may already own (player, server). */
    public static final String WAKE_REQUEST = "WakeRequest";
    /** Upstream: claim an ownerless server, then wake it (player, server). */
    public static final String CLAIM_REQUEST = "ClaimRequest";
    /** Upstream: ask for a fresh {@link #STATUS_UPDATE} for one server (server). */
    public static final String STATUS_QUERY = "StatusQuery";
    /** Downstream: the tile projection (server, phase, ready, players, claimable). */
    public static final String STATUS_UPDATE = "StatusUpdate";
    /** Downstream: a parked player's backend is ready; the lobby may release them (player, server). */
    public static final String TRANSFER_READY = "TransferReady";
    /** Downstream: a refusal (code, message, optional server). */
    public static final String ERROR = "Error";
    /** Upstream (lobby): ask for the current tile list; answered by {@link #LIST_UPDATE}. */
    public static final String LIST_REQUEST = "ListRequest";
    /** Downstream: the user servers the lobby should show, in display order (servers, access). */
    public static final String LIST_UPDATE = "ListUpdate";
    /** Upstream (login gate): the player finished signing in; move them to the lobby (player). */
    public static final String LOGIN_RELEASE = "LoginRelease";

    private final String type;
    private final String player;
    private final String server;
    private final String phase;
    private final boolean ready;
    private final int playersOnline;
    private final int playersMax;
    private final boolean claimable;
    private final String code;
    private final String message;
    private final List<String> servers;
    private final List<String> access;

    private ControlFrame(String type, String player, String server, String phase, boolean ready,
                         int playersOnline, int playersMax, boolean claimable, String code, String message) {
        this(type, player, server, phase, ready, playersOnline, playersMax, claimable, code, message, List.of(), List.of());
    }

    private ControlFrame(String type, String player, String server, String phase, boolean ready,
                         int playersOnline, int playersMax, boolean claimable, String code, String message,
                         List<String> servers, List<String> access) {
        this.type = type;
        this.player = player;
        this.server = server;
        this.phase = phase;
        this.ready = ready;
        this.playersOnline = playersOnline;
        this.playersMax = playersMax;
        this.claimable = claimable;
        this.code = code;
        this.message = message;
        this.servers = servers;
        this.access = access;
    }


    public static ControlFrame wakeRequest(String player, String server) {
        return new ControlFrame(WAKE_REQUEST, player, server, null, false, 0, 0, false, null, null);
    }

    public static ControlFrame claimRequest(String player, String server) {
        return new ControlFrame(CLAIM_REQUEST, player, server, null, false, 0, 0, false, null, null);
    }

    public static ControlFrame statusQuery(String server) {
        return new ControlFrame(STATUS_QUERY, null, server, null, false, 0, 0, false, null, null);
    }

    public static ControlFrame statusUpdate(String server, String phase, boolean ready,
                                            int playersOnline, int playersMax, boolean claimable) {
        return new ControlFrame(STATUS_UPDATE, null, server, phase, ready, playersOnline, playersMax, claimable, null, null);
    }

    public static ControlFrame transferReady(String player, String server) {
        return new ControlFrame(TRANSFER_READY, player, server, null, false, 0, 0, false, null, null);
    }

    /** error reports a refusal; {@code server} is optional (null when not server-scoped). */
    public static ControlFrame error(String code, String message, String server) {
        return new ControlFrame(ERROR, null, server, null, false, 0, 0, false, code, message);
    }

    public static ControlFrame listRequest() {
        return new ControlFrame(LIST_REQUEST, null, null, null, false, 0, 0, false, null, null);
    }

    /** listUpdate carries the tile names; null entries are dropped, the list is copied. */
    public static ControlFrame listUpdate(List<String> servers) {
        return listUpdate(servers, null);
    }

    // Keep verdicts aligned with names; missing verdicts are unknown and extras are discarded.
    public static ControlFrame listUpdate(List<String> servers, List<String> access) {
        List<String> names = new ArrayList<>();
        List<String> verdicts = new ArrayList<>();
        boolean anyKnown = false;
        if (servers != null) {
            for (int i = 0; i < servers.size(); i++) {
                if (servers.get(i) == null) {
                    continue;
                }
                String v = access != null && i < access.size() && access.get(i) != null ? access.get(i) : "";
                names.add(servers.get(i));
                verdicts.add(v);
                anyKnown |= !v.isEmpty();
            }
        }
        return new ControlFrame(LIST_UPDATE, null, null, null, false, 0, 0, false, null, null,
                Collections.unmodifiableList(names),
                anyKnown ? Collections.unmodifiableList(verdicts) : List.of());
    }

    public static ControlFrame loginRelease(String player) {
        return new ControlFrame(LOGIN_RELEASE, player, null, null, false, 0, 0, false, null, null);
    }


    public String type() {
        return type;
    }

    public String player() {
        return player;
    }

    public String server() {
        return server;
    }

    public String phase() {
        return phase;
    }

    public boolean ready() {
        return ready;
    }

    public int playersOnline() {
        return playersOnline;
    }

    public int playersMax() {
        return playersMax;
    }

    public boolean claimable() {
        return claimable;
    }

    public String code() {
        return code;
    }

    public String message() {
        return message;
    }

    /** servers is the {@link #LIST_UPDATE} payload; empty (never null) on every other type. */
    public List<String> servers() {
        return servers;
    }

    /** access is the LIST_UPDATE verdict per server, aligned with servers(); empty when the proxy sent names only, and on every other type. */
    public List<String> access() {
        return access;
    }

    @Override
    public boolean equals(Object o) {
        if (this == o) {
            return true;
        }
        if (!(o instanceof ControlFrame)) {
            return false;
        }
        ControlFrame f = (ControlFrame) o;
        return ready == f.ready
                && playersOnline == f.playersOnline
                && playersMax == f.playersMax
                && claimable == f.claimable
                && Objects.equals(type, f.type)
                && Objects.equals(player, f.player)
                && Objects.equals(server, f.server)
                && Objects.equals(phase, f.phase)
                && Objects.equals(code, f.code)
                && Objects.equals(message, f.message)
                && Objects.equals(servers, f.servers)
                && Objects.equals(access, f.access);
    }

    @Override
    public int hashCode() {
        return Objects.hash(type, player, server, phase, ready, playersOnline, playersMax, claimable, code, message,
                servers, access);
    }

    @Override
    public String toString() {
        return "ControlFrame{" + new String(Control.encode(this), java.nio.charset.StandardCharsets.UTF_8) + "}";
    }
}
