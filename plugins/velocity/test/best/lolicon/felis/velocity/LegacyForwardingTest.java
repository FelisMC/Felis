package best.lolicon.felis.velocity;

import best.lolicon.felis.link.ServerView;

import java.util.List;

/**
 * LegacyForwardingTest covers how a CR's forwarding=legacy label reaches the fork's
 * {@code felis.legacy-forwarding.servers} property (#15): marked servers join and leave
 * it, the startup value stays in as a floor, an unchanged list rewrites nothing, and the
 * report says whether the running proxy acts on the change (stock, a fork that reads
 * the list once, a fork that re-reads it). Detection must not initialize the proxy's
 * connection class. Framework free: a failed assertion throws and the process exits
 * non-zero.
 *
 * <p>Run: {@code javac -d <out> shared/src/main/java/best/lolicon/felis/link/*.java
 * velocity/src/main/java/best/lolicon/felis/velocity/LegacyForwarding.java
 * velocity/test/best/lolicon/felis/velocity/LegacyForwardingTest.java && java -cp <out>
 * best.lolicon.felis.velocity.LegacyForwardingTest}.
 */
public final class LegacyForwardingTest {

    private static int checks;

    public static void main(String[] args) {
        markedServersJoinAndLeaveAboveTheFloor();
        anUnchangedListRewritesNothing();
        aForkThatReadsOnceTakesOnlyTheStartupList();
        stockVelocityReportsMarkedServersAsIgnored();
        detectionTellsTheBuildsApartWithoutInitializing();
        System.out.println("LegacyForwardingTest OK (" + checks + " checks)");
    }

    private static void markedServersJoinAndLeaveAboveTheFloor() {
        System.setProperty(LegacyForwarding.PROPERTY, " legacy18 ,");
        LegacyForwarding lf = new LegacyForwarding(System.getProperty(LegacyForwarding.PROPERTY),
                LegacyForwarding.Proxy.FORK_LIVE);

        LegacyForwarding.Update u = lf.apply(List.of(view("beta", true), view("modern", false), view("alpha", true)));
        assertEq("marked servers join the floor, sorted", "alpha,beta,legacy18", System.getProperty(LegacyForwarding.PROPERTY));
        assertEq("update carries the value", "alpha,beta,legacy18", u.value);
        assertEq("added", List.of("alpha", "beta"), u.added);
        assertEq("nothing removed", List.of(), u.removed);
        assertEq("a live fork acts on it", true, u.effective);
        assertEq("no warning", false, u.warning);

        // beta loses its label; the floor's own server is listed unmarked and stays in.
        u = lf.apply(List.of(view("alpha", true), view("beta", false), view("legacy18", false)));
        assertEq("an unlabelled server leaves, the floor stays", "alpha,legacy18",
                System.getProperty(LegacyForwarding.PROPERTY));
        assertEq("removed", List.of("beta"), u.removed);
        assertEq("nothing added", List.of(), u.added);

        u = lf.apply(List.of());
        assertEq("an empty list falls back to the floor", "legacy18", System.getProperty(LegacyForwarding.PROPERTY));
        assertEq("removed alpha", List.of("alpha"), u.removed);
    }

    private static void anUnchangedListRewritesNothing() {
        System.setProperty(LegacyForwarding.PROPERTY, "legacy18");
        LegacyForwarding lf = new LegacyForwarding("legacy18", LegacyForwarding.Proxy.FORK_LIVE);
        assertEq("floor alone is no change", null, lf.apply(List.of(view("modern", false))));

        lf.apply(List.of(view("old", true)));
        System.setProperty(LegacyForwarding.PROPERTY, "sentinel");
        assertEq("same marks again is no change", null, lf.apply(List.of(view("old", true), view("modern", false))));
        assertEq("the property is left alone", "sentinel", System.getProperty(LegacyForwarding.PROPERTY));

        System.clearProperty(LegacyForwarding.PROPERTY);
        LegacyForwarding none = new LegacyForwarding(null, LegacyForwarding.Proxy.FORK_LIVE);
        assertEq("no floor and no marks is no change", null, none.apply(List.of(view("modern", false))));
        assertEq("the property stays unset", null, System.getProperty(LegacyForwarding.PROPERTY));
        assertEq("a nameless marked entry is skipped", null, none.apply(List.of(view(null, true), view("", true))));
    }

    private static void aForkThatReadsOnceTakesOnlyTheStartupList() {
        LegacyForwarding lf = new LegacyForwarding("legacy18", LegacyForwarding.Proxy.FORK_READS_ONCE);
        LegacyForwarding.Update u = lf.apply(List.of(view("old", true)));
        assertEq("before accepting, the rewrite lands", true, u.effective);
        assertEq("no warning before accepting", false, u.warning);

        lf.accepting();
        u = lf.apply(List.of(view("old", true), view("older", true)));
        assertEq("the property still follows the CRs", "legacy18,old,older", System.getProperty(LegacyForwarding.PROPERTY));
        assertEq("after accepting, it waits for a restart", false, u.effective);
        assertEq("which is a warning", true, u.warning);
        assertEq("naming the restart", true, u.message.contains("Restart felis-velocity"));
    }

    private static void stockVelocityReportsMarkedServersAsIgnored() {
        LegacyForwarding lf = new LegacyForwarding("", LegacyForwarding.Proxy.STOCK);
        LegacyForwarding.Update u = lf.apply(List.of(view("old", true)));
        assertEq("stock never acts on it", false, u.effective);
        assertEq("a marked server on stock is a warning", true, u.warning);
        assertEq("naming the server and the fork", true,
                u.message.contains("[old] keep modern forwarding") && u.message.contains("Felis-Legacy fork"));

        u = lf.apply(List.of(view("old", false)));
        assertEq("dropping a mark on stock is no warning", false, u.warning);
    }

    private static void detectionTellsTheBuildsApartWithoutInitializing() {
        ClassLoader cl = LegacyForwardingTest.class.getClassLoader();
        String p = LegacyForwardingTest.class.getName() + "$";
        assertEq("no connection class", LegacyForwarding.Proxy.STOCK, LegacyForwarding.detect(cl, p + "Absent"));
        assertEq("the real name is absent here too", LegacyForwarding.Proxy.STOCK, LegacyForwarding.detect(cl));
        assertEq("stock connection", LegacyForwarding.Proxy.STOCK, LegacyForwarding.detect(cl, p + "StockConnection"));
        assertEq("0003 alone", LegacyForwarding.Proxy.FORK_READS_ONCE, LegacyForwarding.detect(cl, p + "ReadsOnceConnection"));
        assertEq("0003 + 0004", LegacyForwarding.Proxy.FORK_LIVE, LegacyForwarding.detect(cl, p + "LiveConnection"));
        assertEq("the reload hook alone is not the fork", LegacyForwarding.Proxy.STOCK,
                LegacyForwarding.detect(cl, p + "HookOnlyConnection"));
        assertEq("no connection class was initialized", 0, initialized);
    }

    // ---- fixtures ----

    // Each fixture class counts its own initialization; detect must leave it at zero.
    static int initialized;

    static final class StockConnection {
        static {
            initialized++;
        }

        Object startHandshake() {
            return null;
        }
    }

    static final class ReadsOnceConnection {
        static {
            initialized++;
        }

        Object effectiveForwardingMode() {
            return null;
        }
    }

    static final class LiveConnection {
        static {
            initialized++;
        }

        Object effectiveForwardingMode() {
            return null;
        }

        static java.util.Set<String> felisLegacyForwardingServers() {
            return java.util.Set.of();
        }
    }

    static final class HookOnlyConnection {
        static {
            initialized++;
        }

        static java.util.Set<String> felisLegacyForwardingServers() {
            return java.util.Set.of();
        }
    }

    private static ServerView view(String name, boolean legacy) {
        return new ServerView(name, name, "Running", true, null, "Running", "direct", "10.43.0.9:25565", 0, 20, legacy);
    }

    private static void assertEq(String what, Object want, Object got) {
        if (want == null ? got != null : !want.equals(got)) {
            throw new AssertionError(what + ": want " + want + ", got " + got);
        }
        checks++;
    }
}
