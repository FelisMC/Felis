package best.lolicon.felis.link;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.time.Duration;
import java.util.Comparator;
import java.util.HashMap;
import java.util.Map;
import java.util.stream.Stream;

/**
 * LinkConfigLoaderTest checks how felis-link.properties and the environment combine:
 * the environment wins, both required values must come from somewhere, a first run
 * leaves a template and still refuses to start, and the call timeouts default to
 * 10 s, take the operator's value, and refuse anything that is not a whole number
 * of seconds from 1 to 120.
 *
 * <p>Run: {@code javac -d <out> shared/src/main/java/best/lolicon/felis/link/*.java
 * shared/test/best/lolicon/felis/link/LinkConfigLoaderTest.java && java -cp <out>
 * best.lolicon.felis.link.LinkConfigLoaderTest}.
 */
public final class LinkConfigLoaderTest {

    private static int checks;
    private static Path dir;

    public static void main(String[] args) throws Exception {
        dir = Files.createTempDirectory("felis-link-test");
        try {
            fileAloneIsEnough();
            environmentWinsOverTheFile();
            missingTokenIsRefused();
            firstRunWritesATemplateAndRefuses();
            timeoutsComeFromFileOrEnvironment();
            badTimeoutsAreRefused();
        } finally {
            try (Stream<Path> walk = Files.walk(dir)) {
                walk.sorted(Comparator.reverseOrder()).forEach(p -> p.toFile().delete());
            }
        }
        System.out.println("LinkConfigLoaderTest OK (" + checks + " checks)");
    }

    private static void fileAloneIsEnough() throws IOException {
        Path f = write("plain.properties",
                "api-base-url=http://10.43.0.10:8081/\nservice-token=file-token\n");
        LinkConfig c = LinkConfigLoader.load(f, env());
        assertEq("file url (trailing slash stripped)", "http://10.43.0.10:8081", c.apiBaseUrl());
        assertEq("file token", "file-token", c.serviceToken());
        assertEq("default connect timeout", Duration.ofSeconds(10), c.connectTimeout());
        assertEq("default request timeout", Duration.ofSeconds(10), c.requestTimeout());
    }

    private static void environmentWinsOverTheFile() throws IOException {
        Path f = write("both.properties",
                "api-base-url=http://file:8081\nservice-token=file-token\n");
        LinkConfig c = LinkConfigLoader.load(f, env(
                "FELIS_API_BASE_URL", "http://env:8081",
                "FELIS_SERVICE_TOKEN", "env-token"));
        assertEq("env url", "http://env:8081", c.apiBaseUrl());
        assertEq("env token", "env-token", c.serviceToken());

        // A blank variable is unset, not an override to empty.
        LinkConfig blank = LinkConfigLoader.load(f, env("FELIS_SERVICE_TOKEN", "  "));
        assertEq("blank env token falls back", "file-token", blank.serviceToken());
    }

    private static void missingTokenIsRefused() throws IOException {
        Path f = write("no-token.properties", "api-base-url=http://10.43.0.10:8081\n");
        IOException e = expectRefused("no token", f, env());
        assertContains("no token message", e.getMessage(), "service-token");
    }

    private static void firstRunWritesATemplateAndRefuses() throws IOException {
        Path f = dir.resolve("fresh/felis-link.properties");
        expectRefused("first run", f, env());
        String template = new String(Files.readAllBytes(f), StandardCharsets.UTF_8);
        assertContains("template url key", template, "\napi-base-url=\n");
        assertContains("template token key", template, "\nservice-token=\n");
        assertContains("template connect timeout", template, "\n#connect-timeout-seconds=10\n");
        assertContains("template request timeout", template, "\n#request-timeout-seconds=10\n");
        // The template's commented timeouts leave the defaults in force once filled in.
        Files.write(f, (template.replace("\napi-base-url=\n", "\napi-base-url=http://x:8081\n")
                .replace("\nservice-token=\n", "\nservice-token=t\n")).getBytes(StandardCharsets.UTF_8));
        LinkConfig c = LinkConfigLoader.load(f, env());
        assertEq("filled template connect timeout", Duration.ofSeconds(10), c.connectTimeout());
        assertEq("filled template request timeout", Duration.ofSeconds(10), c.requestTimeout());
    }

    private static void timeoutsComeFromFileOrEnvironment() throws IOException {
        Path f = write("timeouts.properties", "api-base-url=http://x:8081\nservice-token=t\n"
                + "connect-timeout-seconds=3\nrequest-timeout-seconds= 25 \n");
        LinkConfig c = LinkConfigLoader.load(f, env());
        assertEq("file connect timeout", Duration.ofSeconds(3), c.connectTimeout());
        assertEq("file request timeout", Duration.ofSeconds(25), c.requestTimeout());

        LinkConfig e = LinkConfigLoader.load(f, env(
                "FELIS_API_CONNECT_TIMEOUT_SECONDS", "1",
                "FELIS_API_REQUEST_TIMEOUT_SECONDS", "120"));
        assertEq("env connect timeout", Duration.ofSeconds(1), e.connectTimeout());
        assertEq("env request timeout", Duration.ofSeconds(120), e.requestTimeout());
    }

    private static void badTimeoutsAreRefused() throws IOException {
        String base = "api-base-url=http://x:8081\nservice-token=t\n";
        String[][] bad = {
            {"connect-timeout-seconds", "0"},
            {"connect-timeout-seconds", "-5"},
            {"connect-timeout-seconds", "ten"},
            {"request-timeout-seconds", "121"},
            {"request-timeout-seconds", "2.5"},
            {"request-timeout-seconds", "10s"},
        };
        for (String[] kv : bad) {
            Path f = write("bad.properties", base + kv[0] + "=" + kv[1] + "\n");
            IOException e = expectRefused(kv[0] + "=" + kv[1], f, env());
            assertContains(kv[0] + "=" + kv[1] + " names the key", e.getMessage(), kv[0]);
            assertContains(kv[0] + "=" + kv[1] + " quotes the value", e.getMessage(), "\"" + kv[1] + "\"");
        }
        Path f = write("env-bad.properties", base);
        IOException e = expectRefused("env request timeout 0", f, env("FELIS_API_REQUEST_TIMEOUT_SECONDS", "0"));
        assertContains("env bad names the variable", e.getMessage(), "FELIS_API_REQUEST_TIMEOUT_SECONDS");
    }

    // ---- harness ----

    private static java.util.function.UnaryOperator<String> env(String... kv) {
        Map<String, String> m = new HashMap<>();
        for (int i = 0; i < kv.length; i += 2) {
            m.put(kv[i], kv[i + 1]);
        }
        return m::get;
    }

    private static Path write(String name, String body) throws IOException {
        Path f = dir.resolve(name);
        Files.write(f, body.getBytes(StandardCharsets.UTF_8));
        return f;
    }

    private static IOException expectRefused(String what, Path f, java.util.function.UnaryOperator<String> env) {
        try {
            LinkConfigLoader.load(f, env);
        } catch (IOException e) {
            checks++;
            return e;
        }
        throw new AssertionError(what + ": loaded, want an IOException");
    }

    private static void assertContains(String what, String got, String want) {
        if (got == null || !got.contains(want)) {
            throw new AssertionError(what + ": " + got + " does not contain " + want);
        }
        checks++;
    }

    private static void assertEq(String what, Object want, Object got) {
        if (!want.equals(got)) {
            throw new AssertionError(what + ": got " + got + ", want " + want);
        }
        checks++;
    }
}
