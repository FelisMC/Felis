package best.lolicon.felis.link;

import java.nio.charset.StandardCharsets;
import java.util.Arrays;
import java.util.List;

/**
 * ControlRoundTripTest is a hermetic, dependency-free check of the {@code
 * felis:control} codec (spec §12). It lives outside {@code src/main/java} so it
 * never ships in a module jar, and it has no test framework: a failed assertion
 * throws and the process exits non-zero.
 *
 * <p>Because the velocity and paper jars source-share the very {@link Control} and
 * {@link ControlFrame} this test exercises, a passing encode→decode round-trip
 * proves wire compatibility <em>by construction</em>, not merely that the code
 * compiles — the one part of the Java plugin layer that can be verified above
 * "compiles" without a live proxy/lobby. So it asserts field-level equality for
 * every frame type, the {@code type} discriminator on the wire, {@code Error} with
 * its optional {@code server} both present and absent, and that a malformed or
 * unknown frame is rejected, not silently mis-decoded.
 *
 * <p>Run: {@code javac -d <out> shared/src/main/java/best/lolicon/felis/link/*.java
 * shared/test/best/lolicon/felis/link/ControlRoundTripTest.java && java -cp <out>
 * best.lolicon.felis.link.ControlRoundTripTest}.
 */
public final class ControlRoundTripTest {

    private static int checks;

    public static void main(String[] args) {
        roundTripsEveryFrameType();
        wireCarriesTypeDiscriminator();
        wireCarriesRefinedStatusFields();
        errorOmitsServerWhenAbsentButRoundTrips();
        escapesAwkwardStrings();
        listUpdateCarriesNamesInOrder();
        rejectsMalformedAndUnknownFrames();
        System.out.println("ControlRoundTripTest OK (" + checks + " checks)");
    }

    // Every factory frame must survive encode→decode as an equal frame, so the two
    // ends read back exactly what the other wrote.
    private static void roundTripsEveryFrameType() {
        roundTrip(ControlFrame.wakeRequest("Notch", "survival"));
        roundTrip(ControlFrame.claimRequest("Notch", "creative"));
        roundTrip(ControlFrame.statusQuery("survival"));
        roundTrip(ControlFrame.statusUpdate("survival", "Running", true, 3, 20, false));
        roundTrip(ControlFrame.statusUpdate("creative", "Stopped", false, 0, 20, true));
        roundTrip(ControlFrame.transferReady("Notch", "survival"));
        roundTrip(ControlFrame.error("quota_exceeded", "server quota exhausted", "survival"));
        roundTrip(ControlFrame.error("not_linked", "link your account first", null));
        roundTrip(ControlFrame.listRequest());
        roundTrip(ControlFrame.listUpdate(Arrays.asList("alpha", "beta-2", "gamma")));
        roundTrip(ControlFrame.listUpdate(List.of()));
        roundTrip(ControlFrame.loginRelease("Notch"));
    }

    // The discriminator the dispatch switch keys on must appear verbatim on the wire.
    private static void wireCarriesTypeDiscriminator() {
        assertContains(ControlFrame.wakeRequest("p", "s"), "\"type\":\"WakeRequest\"");
        assertContains(ControlFrame.claimRequest("p", "s"), "\"type\":\"ClaimRequest\"");
        assertContains(ControlFrame.statusQuery("s"), "\"type\":\"StatusQuery\"");
        assertContains(ControlFrame.statusUpdate("s", "Running", true, 1, 2, false), "\"type\":\"StatusUpdate\"");
        assertContains(ControlFrame.transferReady("p", "s"), "\"type\":\"TransferReady\"");
        assertContains(ControlFrame.error("c", "m", null), "\"type\":\"Error\"");
        assertContains(ControlFrame.listRequest(), "\"type\":\"ListRequest\"");
        assertContains(ControlFrame.listUpdate(List.of("a")), "\"type\":\"ListUpdate\"");
        assertContains(ControlFrame.loginRelease("p"), "\"type\":\"LoginRelease\"");
    }

    // ListUpdate is the lobby's whole tile set: order is display order and must hold,
    // an empty list stays empty (never null), and non-string entries a hand-written
    // frame might carry are skipped rather than failing the whole list.
    private static void listUpdateCarriesNamesInOrder() {
        ControlFrame f = decode(ControlFrame.listUpdate(Arrays.asList("zeta", "alpha", null, "mid")));
        assertEq("list order (null dropped)", List.of("zeta", "alpha", "mid"), f.servers());
        assertEq("empty list", List.of(), decode(ControlFrame.listUpdate(null)).servers());
        assertEq("servers on a non-list frame", List.of(), decode(ControlFrame.statusQuery("s")).servers());
        ControlFrame mixed = Control.decode(
                "{\"type\":\"ListUpdate\",\"servers\":[\"a\",1,true,\"b\"]}".getBytes(StandardCharsets.UTF_8));
        assertEq("non-string entries skipped", List.of("a", "b"), mixed.servers());
    }

    // StatusUpdate refines the spec's "players" into ready + online + max; the GUI
    // renders all three, so all three must survive the round-trip with exact values.
    private static void wireCarriesRefinedStatusFields() {
        ControlFrame f = decode(ControlFrame.statusUpdate("survival", "Running", true, 7, 40, false));
        assertEq("server", "survival", f.server());
        assertEq("phase", "Running", f.phase());
        assertEq("ready", true, f.ready());
        assertEq("playersOnline", 7, f.playersOnline());
        assertEq("playersMax", 40, f.playersMax());
        assertEq("claimable", false, f.claimable());
        // And the booleans flip independently of one another.
        ControlFrame g = decode(ControlFrame.statusUpdate("creative", "Stopped", false, 0, 8, true));
        assertEq("ready(false)", false, g.ready());
        assertEq("claimable(true)", true, g.claimable());
    }

    // Error's optional server: absent → not on the wire and decodes to null;
    // present → on the wire and decodes back. Both round-trip to an equal frame.
    private static void errorOmitsServerWhenAbsentButRoundTrips() {
        ControlFrame bare = ControlFrame.error("not_linked", "link first", null);
        String wire = new String(Control.encode(bare), StandardCharsets.UTF_8);
        if (wire.contains("\"server\"")) {
            throw new AssertionError("bare Error must not carry a server key: " + wire);
        }
        checks++;
        assertEq("bare Error server", null, decode(bare).server());

        ControlFrame scoped = ControlFrame.error("quota_exceeded", "no room", "survival");
        assertContains(scoped, "\"server\":\"survival\"");
        assertEq("scoped Error server", "survival", decode(scoped).server());
    }

    // Player names and error messages can carry quotes/backslashes/newlines; the
    // hand-rolled writer must escape them so the reader recovers the original.
    private static void escapesAwkwardStrings() {
        String nasty = "a\"b\\c\nd\te";
        ControlFrame f = ControlFrame.error("bad", nasty, "ser\"ver");
        ControlFrame back = decode(f);
        assertEq("escaped message", nasty, back.message());
        assertEq("escaped server", "ser\"ver", back.server());
    }

    // A bad frame is a dropped message, never a crash or a silent mis-decode.
    private static void rejectsMalformedAndUnknownFrames() {
        assertRejected("not json at all".getBytes(StandardCharsets.UTF_8));
        assertRejected("[1,2,3]".getBytes(StandardCharsets.UTF_8)); // root is not an object
        assertRejected("{\"player\":\"p\"}".getBytes(StandardCharsets.UTF_8)); // missing type
        assertRejected("{\"type\":\"Bogus\"}".getBytes(StandardCharsets.UTF_8)); // unknown type
    }

    // ---- harness ----

    private static ControlFrame decode(ControlFrame f) {
        return Control.decode(Control.encode(f));
    }

    private static void roundTrip(ControlFrame f) {
        ControlFrame back = decode(f);
        if (!f.equals(back)) {
            throw new AssertionError("round-trip changed the frame:\n  in:  " + f + "\n  out: " + back);
        }
        checks++;
    }

    private static void assertContains(ControlFrame f, String needle) {
        String wire = new String(Control.encode(f), StandardCharsets.UTF_8);
        if (!wire.contains(needle)) {
            throw new AssertionError("wire " + wire + " is missing " + needle);
        }
        checks++;
    }

    private static void assertEq(String what, Object want, Object got) {
        if (want == null ? got != null : !want.equals(got)) {
            throw new AssertionError(what + " = " + got + ", want " + want);
        }
        checks++;
    }

    private static void assertRejected(byte[] data) {
        try {
            Control.decode(data);
        } catch (IllegalArgumentException expected) {
            checks++;
            return;
        }
        throw new AssertionError("expected rejection of: " + new String(data, StandardCharsets.UTF_8));
    }
}
