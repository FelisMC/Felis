package best.lolicon.felis.velocity;

import best.lolicon.felis.link.LinkException;
import best.lolicon.felis.link.ServerView;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.NoSuchFileException;
import java.nio.file.Path;
import java.nio.file.StandardCopyOption;
import java.util.List;

// Retain registrations on fetch failure; use the atomic disk snapshot once before the first successful refresh.
final class ServerListSource {

    /** Fetch is the one felis-api call the source wraps (FelisApiClient::listServers). */
    interface Fetch {
        List<ServerView> listServers() throws LinkException;
    }

    /** Result is one refresh's outcome. */
    static final class Result {
        /** The list to reconcile against, or null to keep the current registrations. */
        final List<ServerView> servers;
        /** True when servers came from the saved list. */
        final boolean restored;
        /** The fetch failure, or null when the fetch succeeded. */
        final LinkException failure;
        /** Why the saved list could not be written or read, or null. */
        final IOException fileError;

        Result(List<ServerView> servers, boolean restored, LinkException failure, IOException fileError) {
            this.servers = servers;
            this.restored = restored;
            this.failure = failure;
            this.fileError = fileError;
        }
    }

    private final Fetch api;
    private final Path file;

    private boolean fetched;
    private boolean fellBack;
    private String lastSaved;

    ServerListSource(Fetch api, Path file) {
        this.api = api;
        this.file = file;
    }

    /** next fetches the server list and says what the registry should do with it. */
    synchronized Result next() {
        List<ServerView> servers;
        try {
            servers = api.listServers();
        } catch (LinkException e) {
            if (fetched || fellBack) {
                return new Result(null, false, e, null);
            }
            fellBack = true;
            try {
                List<ServerView> saved = load();
                return new Result(saved, saved != null, e, null);
            } catch (IOException io) {
                return new Result(null, false, e, io);
            } catch (IllegalArgumentException bad) {
                return new Result(null, false, e, new IOException("the saved server list is malformed", bad));
            }
        }
        fetched = true;
        try {
            save(servers);
            return new Result(servers, false, null, null);
        } catch (IOException io) {
            return new Result(servers, false, null, io);
        }
    }

    private List<ServerView> load() throws IOException {
        String text;
        try {
            text = Files.readString(file, StandardCharsets.UTF_8);
        } catch (NoSuchFileException absent) {
            return null;
        }
        return ServerView.listFromJson(text);
    }

    private void save(List<ServerView> servers) throws IOException {
        String text = ServerView.listToJson(servers);
        if (text.equals(lastSaved)) {
            return;
        }
        Path tmp = file.resolveSibling(file.getFileName() + ".tmp");
        Files.writeString(tmp, text, StandardCharsets.UTF_8);
        Files.move(tmp, file, StandardCopyOption.REPLACE_EXISTING, StandardCopyOption.ATOMIC_MOVE);
        lastSaved = text;
    }
}
