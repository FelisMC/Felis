package best.lolicon.felis.link;

import java.nio.charset.StandardCharsets;
import java.util.Arrays;
import java.util.List;

public final class ControlRoundTripTest {

    private static int checks;

    public static void main(String[] args) {
        roundTripsEveryFrameType();
        wireCarriesTypeDiscriminator();
        wireCarriesRefinedStatusFields();
        errorOmitsServerWhenAbsentButRoundTrips();
        escapesAwkwardStrings();
        listUpdateCarriesNamesInOrder();
        listUpdateCarriesAccessByName();
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
        roundTrip(ControlFrame.listUpdate(List.of("mine", "theirs"), List.of("owner", "owner_only")));
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

    // Preserve list order and empty lists; skip malformed entries without discarding valid names.
    private static void listUpdateCarriesNamesInOrder() {
        ControlFrame f = decode(ControlFrame.listUpdate(Arrays.asList("zeta", "alpha", null, "mid")));
        assertEq("list order (null dropped)", List.of("zeta", "alpha", "mid"), f.servers());
        assertEq("empty list", List.of(), decode(ControlFrame.listUpdate(null)).servers());
        assertEq("servers on a non-list frame", List.of(), decode(ControlFrame.statusQuery("s")).servers());
        ControlFrame mixed = Control.decode(
                "{\"type\":\"ListUpdate\",\"servers\":[\"a\",1,true,\"b\"]}".getBytes(StandardCharsets.UTF_8));
        assertEq("non-string entries skipped", List.of("a", "b"), mixed.servers());
    }

    // Keep access verdicts aligned when malformed names or verdicts are dropped.
    private static void listUpdateCarriesAccessByName() {
        ControlFrame f = decode(ControlFrame.listUpdate(
                Arrays.asList("mine", null, "theirs", "fresh"),
                Arrays.asList("owner", "wake", "owner_only")));
        assertEq("names", List.of("mine", "theirs", "fresh"), f.servers());
        assertEq("verdicts follow their names", List.of("owner", "owner_only", ""), f.access());
        assertEq("names only: no verdicts", List.of(), decode(ControlFrame.listUpdate(List.of("a"))).access());
        assertEq("all unknown: no verdicts", List.of(),
                decode(ControlFrame.listUpdate(List.of("a", "b"), Arrays.asList("", null))).access());
        assertEq("names only: nothing extra on the wire",
                "{\"type\":\"ListUpdate\",\"servers\":[\"a\"]}",
                new String(Control.encode(ControlFrame.listUpdate(List.of("a"), List.of(""))), StandardCharsets.UTF_8));
        assertEq("extra verdicts are cut", List.of("wake"),
                ControlFrame.listUpdate(List.of("a"), List.of("wake", "owner")).access());
        ControlFrame mixed = Control.decode(("{\"type\":\"ListUpdate\",\"servers\":[\"a\",1,\"b\",\"c\"],"
                + "\"access\":[\"owner\",\"wake\",7,\"allowlist\"]}").getBytes(StandardCharsets.UTF_8));
        assertEq("hand-written: names", List.of("a", "b", "c"), mixed.servers());
        assertEq("hand-written: a skipped name takes its verdict", List.of("owner", "", "allowlist"), mixed.access());
        ControlFrame shortList = Control.decode(("{\"type\":\"ListUpdate\",\"servers\":[\"a\",\"b\"],"
                + "\"access\":[\"retiring\"]}").getBytes(StandardCharsets.UTF_8));
        assertEq("hand-written: a missing verdict is unknown", List.of("retiring", ""), shortList.access());
        assertEq("access on a non-list frame", List.of(), decode(ControlFrame.statusQuery("s")).access());
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
