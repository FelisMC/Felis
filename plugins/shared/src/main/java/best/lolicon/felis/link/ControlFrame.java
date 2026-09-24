package best.lolicon.felis.link;

import java.util.ArrayList;
import java.util.Collections;
import java.util.List;
import java.util.Objects;

/**
 * ControlFrame is one message on the {@code felis:control} plugin-message channel
 * (spec §12): the wire contract between the felis-paper lobby and the Velocity
 * proxy. The lobby is a pure UI face — it holds no felis-api token and maintains no
 * queue — so every lobby action travels to Velocity as one of these frames, and
 * Velocity answers with one back. {@link Control} encodes/decodes them; this class
 * is just the immutable value, source-shared into both the velocity and paper jars
 * so the two ends can never drift on field names.
 *
 * <p>There are nine frame types, discriminated by {@link #type()}:
 * <ul>
 *   <li><b>Upstream from the lobby</b>: {@link #WAKE_REQUEST} and
 *       {@link #CLAIM_REQUEST} carry {@code player}+{@code server};
 *       {@link #STATUS_QUERY} carries {@code server}; {@link #LIST_REQUEST} carries
 *       nothing.</li>
 *   <li><b>Upstream from the login gate</b>: {@link #LOGIN_RELEASE} carries nothing
 *       but the informational {@code player}. It replaces the BungeeCord
 *       {@code Connect} the gate used to send, so {@code bungeecord:main} can be
 *       switched off proxy-wide.</li>
 *   <li><b>Downstream</b> (velocity → lobby): {@link #STATUS_UPDATE} is the tile
 *       projection; {@link #LIST_UPDATE} is the set of tiles to show;
 *       {@link #TRANSFER_READY} tells the lobby a parked player's backend is up;
 *       {@link #ERROR} reports a refusal.</li>
 * </ul>
 *
 * <p>Which upstream types a backend may send is decided by the proxy from the
 * connection they arrive on (the lobby's set, or the login gate's single type); a
 * frame from any other backend is dropped whatever its type.
 *
 * <p>The {@code player} field is informational only on the upstream frames:
 * Velocity derives the real identity from the {@code ServerConnection} the message
 * arrived on, never from this field, so a compromised backend cannot act as another
 * player (spec §14). The lobby still fills it in for symmetry and logging.
 *
 * <p>The spec sketches {@code StatusUpdate} as {@code {server,phase,players,
 * claimable}}; this refines {@code players} into {@link #ready()} +
 * {@link #playersOnline()} + {@link #playersMax()}, mapping the frame 1:1 onto the
 * {@code GET …/menu} endpoint so the GUI can render both the phase button and a
 * "3/20" player count from a single frame.
 *
 * <p>Accessors degrade to {@code null}/{@code 0}/{@code false} for fields absent on
 * a given type, mirroring {@link ServerView}'s tolerant philosophy: a frame is read
 * for the fields its type defines and no others.
 */
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
    /** Downstream: the user servers the lobby should show, in display order (servers). */
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

    private ControlFrame(String type, String player, String server, String phase, boolean ready,
                         int playersOnline, int playersMax, boolean claimable, String code, String message) {
        this(type, player, server, phase, ready, playersOnline, playersMax, claimable, code, message, List.of());
    }

    private ControlFrame(String type, String player, String server, String phase, boolean ready,
                         int playersOnline, int playersMax, boolean claimable, String code, String message,
                         List<String> servers) {
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
    }

    // ---- factories (tolerant: no field validation, so decode can always rebuild) ----

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
        List<String> copy = new ArrayList<>();
        if (servers != null) {
            for (String s : servers) {
                if (s != null) {
                    copy.add(s);
                }
            }
        }
        return new ControlFrame(LIST_UPDATE, null, null, null, false, 0, 0, false, null, null,
                Collections.unmodifiableList(copy));
    }

    public static ControlFrame loginRelease(String player) {
        return new ControlFrame(LOGIN_RELEASE, player, null, null, false, 0, 0, false, null, null);
    }

    // ---- accessors ----

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
                && Objects.equals(servers, f.servers);
    }

    @Override
    public int hashCode() {
        return Objects.hash(type, player, server, phase, ready, playersOnline, playersMax, claimable, code, message,
                servers);
    }

    @Override
    public String toString() {
        return "ControlFrame{" + new String(Control.encode(this), java.nio.charset.StandardCharsets.UTF_8) + "}";
    }
}
