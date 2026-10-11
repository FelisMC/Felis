package best.lolicon.felis.link;

import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.Map;
import java.util.function.Consumer;

/** Player-facing settings on the persistent world volume, edited by the panel. */
public final class ExperienceConfig {
    public static final Path PATH = Path.of("felis-experience.json");
    private final Map<?, ?> values;

    public ExperienceConfig(Map<?, ?> values) {
        this.values = values;
    }

    public static ExperienceConfig load(Path path) throws IOException {
        if (!Files.exists(path)) {
            return new ExperienceConfig(Map.of());
        }
        try {
            Object value = Json.parse(Files.readString(path));
            if (value instanceof Map<?, ?> map) {
                return new ExperienceConfig(map);
            }
            throw new IllegalArgumentException("expected an object");
        } catch (IllegalArgumentException e) {
            throw new IOException("invalid " + path + ": " + e.getMessage(), e);
        }
    }

    /**
     * Loads the settings like {@link #load}, but a file that cannot be read gives the
     * defaults and one warning. The file sits on the world volume where the file manager
     * can edit it by hand, and a typo there must not switch off the login flow or the
     * lobby's protections; the panel shows the damage and saving there repairs it.
     */
    public static ExperienceConfig loadOrDefaults(Path path, Consumer<String> warn) {
        try {
            return load(path);
        } catch (IOException | RuntimeException e) {
            warn.accept("cannot read " + path + " (" + e.getMessage() + "); using the default settings until it is fixed");
            return new ExperienceConfig(Map.of());
        }
    }

    public String text(String key, String fallback) {
        return values.get(key) instanceof String value ? value : fallback;
    }

    public boolean flag(String key, boolean fallback) {
        return values.get(key) instanceof Boolean value ? value : fallback;
    }

    public long number(String key, long fallback) {
        return values.get(key) instanceof Number value ? value.longValue() : fallback;
    }
}
