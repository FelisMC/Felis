package best.lolicon.felis.link;

import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;

/**
 * ExperienceConfigTest pins that a settings file broken by hand (in the file manager)
 * costs only the customizations: the login gate and the lobby run on the defaults with one
 * warning, instead of switching the login flow or the lobby's protections off.
 *
 * <p>Run: {@code javac -d <out> shared/src/main/java/best/lolicon/felis/link/*.java
 * shared/test/best/lolicon/felis/link/ExperienceConfigTest.java && java -cp <out>
 * best.lolicon.felis.link.ExperienceConfigTest}.
 */
public final class ExperienceConfigTest {

    private static int checks;

    public static void main(String[] args) throws Exception {
        Path dir = Files.createTempDirectory("felis-experience");
        Path file = dir.resolve("felis-experience.json");
        List<String> warnings = new ArrayList<>();
        try {
            ExperienceConfig absent = ExperienceConfig.loadOrDefaults(file, warnings::add);
            assertEq("no file: defaults", "Felis Login", absent.text("bookTitle", "Felis Login"));
            assertEq("no file: no warning", 0, warnings.size());

            Files.writeString(file, "{\"bookTitle\":\"My Network\",\"openBook\":false}");
            ExperienceConfig good = ExperienceConfig.loadOrDefaults(file, warnings::add);
            assertEq("a good file is read", "My Network", good.text("bookTitle", "Felis Login"));
            assertEq("a good flag is read", false, good.flag("openBook", true));
            assertEq("a good file: no warning", 0, warnings.size());

            for (String broken : new String[] {"{\"bookTitle\": \"My Net", "[1,2]", "{} trailing", "\"text\""}) {
                warnings.clear();
                Files.writeString(file, broken);
                ExperienceConfig damaged = ExperienceConfig.loadOrDefaults(file, warnings::add);
                assertEq("damaged <" + broken + ">: defaults", "Felis Login", damaged.text("bookTitle", "Felis Login"));
                assertEq("damaged <" + broken + ">: the book still opens", true, damaged.flag("openBook", true));
                assertEq("damaged <" + broken + ">: one warning", 1, warnings.size());
                assertEq("damaged <" + broken + ">: the warning names the file", true, warnings.get(0).contains("felis-experience.json"));
            }

            boolean strict = false;
            try {
                ExperienceConfig.load(file);
            } catch (java.io.IOException e) {
                strict = true;
            }
            assertEq("load itself still reports the damage", true, strict);
        } finally {
            Files.deleteIfExists(file);
            Files.deleteIfExists(dir);
        }
        System.out.println("ExperienceConfigTest: " + checks + " checks passed");
    }

    private static void assertEq(String what, Object want, Object got) {
        checks++;
        if (!want.equals(got)) {
            throw new AssertionError(what + ": want " + want + ", got " + got);
        }
    }
}
