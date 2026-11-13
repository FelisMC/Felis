package best.lolicon.felis.velocity;

import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;

// The mode key decides whether this proxy routes Felis servers or only hands sign-in to Felis-nano. Framework free.
public final class FelisVelocityConfigTest {

    private static int checks;

    public static void main(String[] args) throws Exception {
        Path dir = Files.createTempDirectory("felis-config");
        String base = "api-base-url=http://127.0.0.1:8081\nservice-token=t\n";

        assertEq("no mode: a full Felis proxy", false, load(dir, base).nano());
        assertEq("mode=nano: sign-in only", true, load(dir, base + "mode=nano\n").nano());
        assertEq("the mode ignores case and spaces", true, load(dir, base + "mode = NANO \n").nano());
        try {
            load(dir, base + "mode=lobby\n");
            throw new AssertionError("an unknown mode loaded");
        } catch (IOException e) {
            assertEq("an unknown mode names the key", true, e.getMessage().contains("mode must be \"nano\""));
        }

        String named = base + "root-domain=example.org\n";
        FelisVelocityConfig direct = load(dir, named + "panel-port=30443\n");
        assertEq("direct access: the panel link carries the NodePort", "https://console.example.org:30443", direct.panelUrl());
        assertEq("direct access: the operator link carries it too", "https://op.console.example.org:30443", direct.adminUrl());
        Files.writeString(dir.resolve("felis-link.properties"), named + "panel-port=\n");
        assertEq("a proxied panel drops the port without a restart", "https://console.example.org", direct.panelUrl());
        System.out.println("FelisVelocityConfigTest OK (" + checks + " checks)");
    }

    private static FelisVelocityConfig load(Path dir, String properties) throws IOException {
        Path file = dir.resolve("felis-link.properties");
        Files.writeString(file, properties);
        return FelisVelocityConfig.load(file, msg -> { });
    }

    private static void assertEq(String what, Object want, Object got) {
        if (!want.equals(got)) {
            throw new AssertionError(what + ": got " + got + ", want " + want);
        }
        checks++;
    }
}
