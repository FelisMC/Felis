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

/**
 * FileToken is a service token read from felis-link.properties that follows the
 * file while the process runs. {@code felis rotate-token velocity} rewrites the
 * file (a rename, so it is never seen half-written) and waits for the notice this
 * gives; the proxy presents the new token from its next felis-api call and keeps
 * every player connected, where a restart would have dropped them all.
 *
 * <p>The file is read again at most once every {@link #RECHECK_NANOS} ns, on the
 * calls themselves. A file that cannot be read, or holds no token, leaves the
 * current token in place: an operator's half-finished edit never blanks a token
 * that works.
 */
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

    /**
     * fingerprint names a token in a log line without giving it away: the first 12
     * hex digits of its SHA-256, which {@code felis rotate-token} computes the same
     * way to recognise the reload it is waiting for.
     */
    static String fingerprint(String token) {
        try {
            byte[] sum = MessageDigest.getInstance("SHA-256").digest(token.getBytes(StandardCharsets.UTF_8));
            return HexFormat.of().formatHex(sum).substring(0, 12);
        } catch (NoSuchAlgorithmException e) {
            throw new IllegalStateException("SHA-256 is missing from this JVM", e);
        }
    }
}
