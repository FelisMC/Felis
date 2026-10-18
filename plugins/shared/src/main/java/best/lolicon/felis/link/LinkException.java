package best.lolicon.felis.link;

// Preserve status and stable error codes for branching, but never echo internal failures into player chat.
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
