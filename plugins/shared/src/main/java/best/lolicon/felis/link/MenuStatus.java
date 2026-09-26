package best.lolicon.felis.link;

import java.util.Map;

/**
 * MenuStatus is the proxy-side mirror of the felis-api lobby menu projection
 * ({@code GET /api/v1/internal/servers/{name}/menu}, spec §12). It is deliberately
 * <em>not</em> {@link ServerView}: the menu endpoint adds one field the §11 lifecycle
 * views never carry — {@link #claimable()}, derived from ownership (an ownerless
 * server can be claimed) — and drops the routing-only fields (subdomain, endpoint
 * address, desiredState) the GUI has no use for. Keeping it a separate type means a
 * future change to either contract can't silently corrupt the other.
 *
 * <p>Velocity reads this for a {@code StatusQuery} and projects it onto a
 * {@link ControlFrame#STATUS_UPDATE} frame the felis-paper lobby renders as a tile:
 * {@code ready} and {@code claimable}, with felis-api's per-player verdict from the
 * {@code ListUpdate}, choose the button (Join / Claim&nbsp;&amp;&nbsp;Start / Start, or
 * a grey tile naming why not), {@code phase} is the status line, and
 * {@code playersOnline}/{@code playersMax} render the "3/20" count.
 *
 * <p>{@link #fromJson(Map)} is tolerant in the same way as {@link ServerView}: an
 * absent field degrades to null/zero/false rather than throwing, so a partial body
 * can never crash the proxy's event thread.
 */
public final class MenuStatus {
    private final String name;
    private final String phase;
    private final boolean ready;
    private final int playersOnline;
    private final int playersMax;
    private final boolean claimable;

    public MenuStatus(String name, String phase, boolean ready,
                      int playersOnline, int playersMax, boolean claimable) {
        this.name = name;
        this.phase = phase;
        this.ready = ready;
        this.playersOnline = playersOnline;
        this.playersMax = playersMax;
        this.claimable = claimable;
    }

    /** fromJson builds a status from a parsed menu object, tolerating absent fields. */
    public static MenuStatus fromJson(Map<?, ?> o) {
        return new MenuStatus(
                str(o, "name"),
                str(o, "phase"),
                bool(o, "ready"),
                intval(o, "playersOnline"),
                intval(o, "playersMax"),
                bool(o, "claimable"));
    }

    public String name() {
        return name;
    }

    public String phase() {
        return phase;
    }

    public boolean ready() {
        return ready;
    }

    public int playersOnline() {
        return playersOnline;
    }

    public int playersMax() {
        return playersMax;
    }

    /** claimable is true when the server has no owner yet (the menu's `Claim & Start`). */
    public boolean claimable() {
        return claimable;
    }

    private static String str(Map<?, ?> o, String key) {
        Object v = o.get(key);
        return v instanceof String ? (String) v : null;
    }

    private static boolean bool(Map<?, ?> o, String key) {
        Object v = o.get(key);
        return v instanceof Boolean && (Boolean) v;
    }

    private static int intval(Map<?, ?> o, String key) {
        Object v = o.get(key);
        // Json parses every number as Double; the player counts are int32 server-side.
        return v instanceof Number ? ((Number) v).intValue() : 0;
    }
}
