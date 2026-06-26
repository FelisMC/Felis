package best.lolicon.felis.link;

/**
 * LinkException is thrown when a link-code request does not succeed. It carries
 * the HTTP status (0 for a transport/timeout failure that produced no response)
 * and the server's stable {@code error.code} when one was returned, so callers
 * can branch on a machine-readable cause without parsing human text. The message
 * is safe to log server-side; the plugins deliberately do <em>not</em> echo it to
 * the player, surfacing a generic "try again" line instead so nothing internal
 * leaks into chat.
 */
public final class LinkException extends Exception {
    private static final long serialVersionUID = 1L;

    private final int statusCode;
    private final String errorCode;

    public LinkException(int statusCode, String errorCode, String message) {
        super(message);
        this.statusCode = statusCode;
        this.errorCode = errorCode;
    }

    public LinkException(int statusCode, String errorCode, String message, Throwable cause) {
        super(message, cause);
        this.statusCode = statusCode;
        this.errorCode = errorCode;
    }

    /** statusCode is the HTTP status, or 0 if the request never completed. */
    public int statusCode() {
        return statusCode;
    }

    /** errorCode is the server's stable error.code, or null when unavailable. */
    public String errorCode() {
        return errorCode;
    }
}
