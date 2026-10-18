package best.lolicon.felis.link;

import java.io.IOException;
import java.io.InputStream;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.util.HexFormat;
import java.util.Properties;
import java.util.function.Consumer;
import java.util.function.LongSupplier;
import java.util.function.Supplier;

// Reload atomically rotated token files without disconnecting players.
// Recheck at most once per interval; unreadable or empty files preserve the current token.
final class FileToken implements Supplier<String> {
    static final long RECHECK_NANOS = 1_000_000_000L;

    private final Path file;
    private final String key;
    private final LongSupplier clock;
    private final Consumer<String> notice;
    private String current;
    private long checkedAt;

    FileToken(Path file, String key, String initial, LongSupplier clock, Consumer<String> notice) {
        this.file = file;
        this.key = key;
        this.current = initial;
        this.clock = clock;
        this.notice = notice;
        this.checkedAt = clock.getAsLong();
    }

    @Override
    public synchronized String get() {
        long now = clock.getAsLong();
        if (now - checkedAt < RECHECK_NANOS) {
            return current;
        }
        checkedAt = now;
        String read = read();
        if (read != null && !read.equals(current)) {
            current = read;
            notice.accept("service-token reloaded from " + file + " (fingerprint " + fingerprint(read) + ")");
        }
        return current;
    }

    private String read() {
        Properties props = new Properties();
        try (InputStream in = Files.newInputStream(file)) {
            props.load(in);
        } catch (IOException | IllegalArgumentException e) {
            return null;
        }
        String v = props.getProperty(key);
        return v == null || v.trim().isEmpty() ? null : v;
    }

    // Log only the first 12 SHA-256 hex digits, matching felis rotate-token.
    static String fingerprint(String token) {
        try {
            byte[] sum = MessageDigest.getInstance("SHA-256").digest(token.getBytes(StandardCharsets.UTF_8));
            return HexFormat.of().formatHex(sum).substring(0, 12);
        } catch (NoSuchAlgorithmException e) {
            throw new IllegalStateException("SHA-256 is missing from this JVM", e);
        }
    }
}
