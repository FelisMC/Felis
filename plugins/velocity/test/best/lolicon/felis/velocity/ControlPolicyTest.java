package best.lolicon.felis.velocity;

import best.lolicon.felis.link.ControlFrame;

import java.util.Set;

/**
 * ControlPolicyTest pins down who may send what on {@code felis:control}. It needs no
 * Velocity classes: {@link ControlPolicy} takes the source server's name and a
 * managed-server predicate, which is everything the proxy knows that a backend cannot
 * forge. Framework free: a failed assertion throws and the process exits non-zero.
 *
 * <p>Run: {@code javac -d <out> shared/src/main/java/best/lolicon/felis/link/*.java
 * velocity/src/main/java/best/lolicon/felis/velocity/ControlPolicy.java
 * velocity/test/best/lolicon/felis/velocity/ControlPolicyTest.java && java -cp <out>
 * best.lolicon.felis.velocity.ControlPolicyTest}.
 */
public final class ControlPolicyTest {

    private static int checks;

    private static final Set<String> MANAGED = Set.of("login", "lobby", "alpha", "beta", "victim");
    private static final ControlPolicy POLICY = new ControlPolicy("login", "lobby", MANAGED::contains);

    public static void main(String[] args) {
        lobbyMayDriveTheMenu();
        loginMayOnlyRelease();
        userBackendsAreRefusedWhateverTheySend();
        menuFramesMustNameAManagedUserServer();
        System.out.println("ControlPolicyTest OK (" + checks + " checks)");
    }

    private static void lobbyMayDriveTheMenu() {
        expect("lobby StatusQuery", ControlPolicy.Verdict.ACCEPT, "lobby", ControlFrame.statusQuery("alpha"));
        expect("lobby WakeRequest", ControlPolicy.Verdict.ACCEPT, "lobby", ControlFrame.wakeRequest("p", "alpha"));
        expect("lobby ClaimRequest", ControlPolicy.Verdict.ACCEPT, "lobby", ControlFrame.claimRequest("p", "beta"));
        expect("lobby ListRequest", ControlPolicy.Verdict.ACCEPT, "lobby", ControlFrame.listRequest());
        expect("source name case", ControlPolicy.Verdict.ACCEPT, "LOBBY", ControlFrame.listRequest());
        // The lobby has no business releasing players from the gate.
        expect("lobby LoginRelease", ControlPolicy.Verdict.TYPE_NOT_ALLOWED, "lobby", ControlFrame.loginRelease("p"));
        // Downstream-only types arriving upstream are not actionable.
        expect("lobby StatusUpdate", ControlPolicy.Verdict.TYPE_NOT_ALLOWED, "lobby",
                ControlFrame.statusUpdate("alpha", "Running", true, 1, 2, false));
        expect("lobby Error", ControlPolicy.Verdict.TYPE_NOT_ALLOWED, "lobby", ControlFrame.error("x", "y", null));
    }

    private static void loginMayOnlyRelease() {
        expect("login LoginRelease", ControlPolicy.Verdict.ACCEPT, "login", ControlFrame.loginRelease("p"));
        expect("login WakeRequest", ControlPolicy.Verdict.TYPE_NOT_ALLOWED, "login",
                ControlFrame.wakeRequest("p", "alpha"));
        expect("login ClaimRequest", ControlPolicy.Verdict.TYPE_NOT_ALLOWED, "login",
                ControlFrame.claimRequest("p", "alpha"));
        expect("login ListRequest", ControlPolicy.Verdict.TYPE_NOT_ALLOWED, "login", ControlFrame.listRequest());
    }

    // The reported hole: a server owner's plugin on "alpha" sending menu frames for
    // whoever stands there, or releasing players from the gate.
    private static void userBackendsAreRefusedWhateverTheySend() {
        for (ControlFrame f : new ControlFrame[] {
                ControlFrame.claimRequest("visitor", "beta"),
                ControlFrame.wakeRequest("visitor", "victim"),
                ControlFrame.statusQuery("alpha"),
                ControlFrame.listRequest(),
                ControlFrame.loginRelease("visitor"),
        }) {
            expect("alpha " + f.type(), ControlPolicy.Verdict.FOREIGN_SOURCE, "alpha", f);
            expect("unmanaged " + f.type(), ControlPolicy.Verdict.FOREIGN_SOURCE, "somewhere", f);
            expect("null source " + f.type(), ControlPolicy.Verdict.FOREIGN_SOURCE, null, f);
        }
    }

    private static void menuFramesMustNameAManagedUserServer() {
        String[] bad = {
            null, "", "victim/join-event?", "victim/join-event", "Victim", "unknown",
            "login", "lobby", // system servers are not menu targets
            "-x-", "a b",
        };
        for (String server : bad) {
            expect("wake " + server, ControlPolicy.Verdict.BAD_SERVER, "lobby", ControlFrame.wakeRequest("p", server));
            expect("claim " + server, ControlPolicy.Verdict.BAD_SERVER, "lobby", ControlFrame.claimRequest("p", server));
            expect("status " + server, ControlPolicy.Verdict.BAD_SERVER, "lobby", ControlFrame.statusQuery(server));
        }
        assertTrue("user server listed", POLICY.isUserServer("alpha"));
        assertTrue("lobby not listed", !POLICY.isUserServer("lobby"));
        assertTrue("wellFormed ok", ControlPolicy.wellFormed("abc-1"));
        assertTrue("wellFormed slash", !ControlPolicy.wellFormed("a/b"));
    }

    // ---- harness ----

    private static void expect(String what, ControlPolicy.Verdict want, String source, ControlFrame f) {
        ControlPolicy.Verdict got = POLICY.check(source, f);
        if (got != want) {
            throw new AssertionError(what + ": " + got + ", want " + want);
        }
        checks++;
    }

    private static void assertTrue(String what, boolean v) {
        if (!v) {
            throw new AssertionError(what);
        }
        checks++;
    }
}
