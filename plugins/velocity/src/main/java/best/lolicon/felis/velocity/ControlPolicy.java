package best.lolicon.felis.velocity;

import best.lolicon.felis.link.ControlFrame;

import java.util.Set;
import java.util.function.Predicate;
import java.util.regex.Pattern;

/**
 * ControlPolicy decides whether a {@code felis:control} frame is acted on, from two
 * facts the proxy knows and the sender cannot forge: which backend connection the
 * frame arrived on, and which servers the registry currently manages. It holds no
 * Velocity types so the whole decision is unit-testable (ControlPolicyTest).
 *
 * <p>Every user backend runs code its owner chose, so "arrived from a backend" says
 * nothing about who wrote the frame. Before this gate any server owner could install
 * a plugin that sent ClaimRequests and WakeRequests on behalf of whoever was standing
 * on their server, and pick the {@code server} field freely. The rules are now:
 *
 * <ul>
 *   <li>the lobby may send {@code StatusQuery}, {@code WakeRequest},
 *       {@code ClaimRequest} and {@code ListRequest};</li>
 *   <li>the login gate may send {@code LoginRelease} and nothing else;</li>
 *   <li>any other backend is refused outright, whatever the type.</li>
 * </ul>
 *
 * <p>A frame that names a server must name a user server the registry manages and
 * that is well-formed by the platform's naming rule. The two system servers are not
 * menu targets: the lobby never lists them, and waking or claiming either is not a
 * player action.
 */
final class ControlPolicy {

    /** The outcome of {@link #check}; everything but ACCEPT means drop the frame. */
    enum Verdict {
        ACCEPT,
        /** The frame came from a backend that is neither the lobby nor the login gate. */
        FOREIGN_SOURCE,
        /** The source may use the channel, but not for this frame type. */
        TYPE_NOT_ALLOWED,
        /** The frame names no server, a malformed one, a system one or an unknown one. */
        BAD_SERVER
    }

    // Mirrors internal/naming.serverNameRE and its no-leading/trailing-dash rule.
    private static final Pattern SERVER_NAME = Pattern.compile("^[a-z0-9][a-z0-9-]{1,30}[a-z0-9]$");

    private static final Set<String> LOBBY_TYPES = Set.of(
            ControlFrame.STATUS_QUERY,
            ControlFrame.WAKE_REQUEST,
            ControlFrame.CLAIM_REQUEST,
            ControlFrame.LIST_REQUEST);

    private final String loginServer;
    private final String lobbyServer;
    private final Predicate<String> managed;

    ControlPolicy(String loginServer, String lobbyServer, Predicate<String> managed) {
        this.loginServer = loginServer;
        this.lobbyServer = lobbyServer;
        this.managed = managed;
    }

    Verdict check(String sourceServer, ControlFrame frame) {
        if (sourceServer == null) {
            return Verdict.FOREIGN_SOURCE;
        }
        if (sourceServer.equalsIgnoreCase(loginServer)) {
            return ControlFrame.LOGIN_RELEASE.equals(frame.type()) ? Verdict.ACCEPT : Verdict.TYPE_NOT_ALLOWED;
        }
        if (!sourceServer.equalsIgnoreCase(lobbyServer)) {
            return Verdict.FOREIGN_SOURCE;
        }
        if (!LOBBY_TYPES.contains(frame.type())) {
            return Verdict.TYPE_NOT_ALLOWED;
        }
        if (ControlFrame.LIST_REQUEST.equals(frame.type())) {
            return Verdict.ACCEPT;
        }
        return isUserServer(frame.server()) ? Verdict.ACCEPT : Verdict.BAD_SERVER;
    }

    /** wellFormed is the platform naming rule alone, with no registry lookup. */
    static boolean wellFormed(String name) {
        return name != null && SERVER_NAME.matcher(name).matches();
    }

    /** isUserServer is true for a well-formed, managed server that is not login or lobby. */
    boolean isUserServer(String name) {
        return wellFormed(name)
                && !name.equalsIgnoreCase(loginServer)
                && !name.equalsIgnoreCase(lobbyServer)
                && managed.test(name);
    }
}
