package best.lolicon.felis.velocity;

import best.lolicon.felis.link.LinkException;
import best.lolicon.felis.link.ServerView;

import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.Arrays;
import java.util.Comparator;
import java.util.List;
import java.util.stream.Stream;

/**
 * ServerListSourceTest covers what the proxy routes on while felis-api is down: a
 * successful fetch is saved and always wins, a failure keeps the current
 * registrations, and only before the first success does the saved list come back —
 * field for field, once. Framework free: a failed assertion throws and the process
 * exits non-zero.
 *
 * <p>Run: {@code javac -d <out> shared/src/main/java/best/lolicon/felis/link/*.java
 * velocity/src/main/java/best/lolicon/felis/velocity/ServerListSource.java
 * velocity/test/best/lolicon/felis/velocity/ServerListSourceTest.java && java -cp <out>
 * best.lolicon.felis.velocity.ServerListSourceTest}.
 */
public final class ServerListSourceTest {

    private static int checks;

    // What the stub felis-api does next: answer with this list, or fail when null.
    private static volatile List<ServerView> answer;

    private static final ServerView LOGIN = new ServerView("login", null, "Running", true,
            null, "Running", "ClusterIP", "10.43.0.17:25565", 0, 0);
    private static final ServerView SURVIVAL = new ServerView("survival", "Survival", "Stopped", false,
            "Anyone", "Stopped", "ClusterIP", "10.43.9.2:25565", 3, 20);

    public static void main(String[] args) throws Exception {
        Path dir = Files.createTempDirectory("felis-server-list");
        try {
            restartDuringAnOutageRoutesOnTheSavedList(dir.resolve("a"));
            failuresAfterAFetchKeepTheCurrentRegistrations(dir.resolve("b"));
            theSavedListIsHandedOutOnce(dir.resolve("c"));
            noSavedListMeansNothingToRestore(dir.resolve("d"));
            aMalformedSavedListIsReportedAndIgnored(dir.resolve("e"));
            theLegacyForwardingMarkIsSaved(dir.resolve("f"));
            theApiEnvelopeParses();
        } finally {
            try (Stream<Path> walk = Files.walk(dir)) {
                walk.sorted(Comparator.reverseOrder()).forEach(p -> p.toFile().delete());
            }
        }
        System.out.println("ServerListSourceTest OK (" + checks + " checks)");
    }

    private static Path fresh(Path dir) throws Exception {
        Files.createDirectories(dir);
        return dir.resolve("last-servers.json");
    }

    private static void restartDuringAnOutageRoutesOnTheSavedList(Path dir) throws Exception {
        Path file = fresh(dir);
        answer = Arrays.asList(LOGIN, SURVIVAL);
        ServerListSource.Result live = new ServerListSource(ServerListSourceTest::fetch, file).next();
        assertEq("live list returned", 2, live.servers.size());
        assertEq("live list is not restored", false, live.restored);
        assertEq("live fetch has no failure", null, live.failure);
        assertEq("saved without error", null, live.fileError);
        assertEq("no temp file left behind", List.of("last-servers.json"), names(dir));

        // A new proxy process (a new source) while felis-api is down.
        answer = null;
        ServerListSource.Result r = new ServerListSource(ServerListSourceTest::fetch, file).next();
        assertEq("restored", true, r.restored);
        assertEq("failure is reported", 503, r.failure.statusCode());
        assertEq("two backends", 2, r.servers.size());
        ServerView login = r.servers.get(0);
        assertEq("login name", "login", login.name());
        assertEq("login subdomain stays absent", null, login.subdomain());
        assertEq("login address", "10.43.0.17:25565", login.endpointAddress());
        assertEq("login ready", true, login.ready());
        ServerView s = r.servers.get(1);
        assertEq("survival name", "survival", s.name());
        assertEq("survival subdomain", "Survival", s.subdomain());
        assertEq("survival phase", "Stopped", s.phase());
        assertEq("survival ready", false, s.ready());
        assertEq("survival policy", "Anyone", s.autostartPolicy());
        assertEq("survival desired", "Stopped", s.desiredState());
        assertEq("survival mode", "ClusterIP", s.endpointMode());
        assertEq("survival address", "10.43.9.2:25565", s.endpointAddress());
        assertEq("survival online", 3, s.playersOnline());
        assertEq("survival max", 20, s.playersMax());
    }

    private static void failuresAfterAFetchKeepTheCurrentRegistrations(Path dir) throws Exception {
        Path file = fresh(dir);
        ServerListSource src = new ServerListSource(ServerListSourceTest::fetch, file);
        answer = List.of(LOGIN);
        src.next();
        answer = null;
        ServerListSource.Result r = src.next();
        assertEq("keep current registrations", null, r.servers);
        assertEq("not restored", false, r.restored);
        assertEq("failure reported", 503, r.failure.statusCode());

        // A later success replaces the saved list, including a removal.
        answer = List.of(SURVIVAL);
        src.next();
        answer = null;
        ServerListSource.Result restarted = new ServerListSource(ServerListSourceTest::fetch, file).next();
        assertEq("newest list saved", 1, restarted.servers.size());
        assertEq("newest list content", "survival", restarted.servers.get(0).name());
    }

    private static void theSavedListIsHandedOutOnce(Path dir) throws Exception {
        Path file = fresh(dir);
        answer = List.of(LOGIN);
        new ServerListSource(ServerListSourceTest::fetch, file).next();
        answer = null;
        ServerListSource src = new ServerListSource(ServerListSourceTest::fetch, file);
        assertEq("first failure restores", true, src.next().restored);
        ServerListSource.Result again = src.next();
        assertEq("second failure keeps registrations", null, again.servers);
        assertEq("second failure is not a restore", false, again.restored);
    }

    private static void noSavedListMeansNothingToRestore(Path dir) throws Exception {
        Path file = fresh(dir);
        answer = null;
        ServerListSource.Result r = new ServerListSource(ServerListSourceTest::fetch, file).next();
        assertEq("nothing to route on", null, r.servers);
        assertEq("not restored", false, r.restored);
        assertEq("a missing file is no error", null, r.fileError);
    }

    private static void aMalformedSavedListIsReportedAndIgnored(Path dir) throws Exception {
        Path file = fresh(dir);
        Files.writeString(file, "{\"servers\":[{\"name\":\"login\"", StandardCharsets.UTF_8);
        answer = null;
        ServerListSource.Result r = new ServerListSource(ServerListSourceTest::fetch, file).next();
        assertEq("nothing to route on", null, r.servers);
        assertEq("not restored", false, r.restored);
        assertEq("malformed file reported", true, r.fileError != null);

        // The next success overwrites it.
        answer = List.of(LOGIN);
        ServerListSource src = new ServerListSource(ServerListSourceTest::fetch, file);
        src.next();
        assertEq("rewritten", true, Files.readString(file, StandardCharsets.UTF_8).contains("\"10.43.0.17:25565\""));
    }

    // A proxy restarted during an outage must keep forwarding a marked 1.8 backend the
    // legacy way (#15), so the mark rides the saved list like every other field.
    private static void theLegacyForwardingMarkIsSaved(Path dir) throws Exception {
        Path file = fresh(dir);
        ServerView legacy = new ServerView("legacy18", "old", "Running", true,
                null, "Running", "ClusterIP", "10.43.7.1:25565", 0, 20, true);
        answer = List.of(legacy, SURVIVAL);
        new ServerListSource(ServerListSourceTest::fetch, file).next();
        answer = null;
        ServerListSource.Result r = new ServerListSource(ServerListSourceTest::fetch, file).next();
        assertEq("restored", true, r.restored);
        assertEq("the mark survives", true, r.servers.get(0).legacyForwarding());
        assertEq("an unmarked server stays unmarked", false, r.servers.get(1).legacyForwarding());
    }

    // The exact shape GET /api/v1/servers answers with, extra fields included.
    private static void theApiEnvelopeParses() {
        List<ServerView> v = ServerView.listFromJson("{\"servers\":[{\"name\":\"lobby\",\"subdomain\":\"\","
                + "\"phase\":\"Running\",\"ready\":true,\"desiredState\":\"Running\",\"endpointMode\":\"ClusterIP\","
                + "\"endpointAddress\":\"10.43.1.5:25565\",\"playersOnline\":1,\"playersMax\":100,"
                + "\"owner\":\"x\"},\"junk\",{\"name\":\"legacy18\",\"legacyForwarding\":true}]}");
        assertEq("non-object entries skipped", 2, v.size());
        assertEq("lobby name", "lobby", v.get(0).name());
        assertEq("lobby max", 100, v.get(0).playersMax());
        assertEq("an absent mark is false", false, v.get(0).legacyForwarding());
        assertEq("the mark parses", true, v.get(1).legacyForwarding());
        try {
            ServerView.listFromJson("[]");
        } catch (IllegalArgumentException expected) {
            checks++;
            return;
        }
        throw new AssertionError("a bare array is not a server list");
    }

    // ---- harness ----

    private static List<ServerView> fetch() throws LinkException {
        List<ServerView> a = answer;
        if (a == null) {
            throw new LinkException(503, "unavailable", "stub outage");
        }
        return a;
    }

    private static List<String> names(Path dir) throws Exception {
        try (Stream<Path> s = Files.list(dir)) {
            return s.map(p -> p.getFileName().toString()).sorted().toList();
        }
    }

    private static void assertEq(String what, Object want, Object got) {
        if (want == null ? got != null : !want.equals(got)) {
            throw new AssertionError(what + ": want " + want + ", got " + got);
        }
        checks++;
    }
}
