package best.lolicon.felis.link;

import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;

/**
 * Control is the {@code felis:control} plugin-message codec (spec §12): it turns a
 * {@link ControlFrame} into the raw bytes a plugin message carries, and back. Both
 * the Velocity proxy and the felis-paper lobby source-share this class, so the wire
 * format has exactly one definition and the two ends cannot drift.
 *
 * <p>Frames are encoded as <b>raw UTF-8 JSON bytes</b>, not via
 * {@code DataOutputStream.writeUTF}. The namespaced channel hands the listener the
 * exact byte array on both ends — Velocity's {@code PluginMessageEvent.getData()}
 * and Bukkit's {@code PluginMessageListener} — so there is no length-prefix framing
 * to agree on, and the 64&nbsp;KB ceiling {@code writeUTF} imposes is avoided.
 *
 * <p>The codec reuses the package-private {@link Json} reader (which is why this
 * lives in {@code best.lolicon.felis.link}) and a tiny hand-rolled writer, keeping
 * the shared core dependency-free. Decoding is strict on structure (a non-object or
 * a missing/unknown {@code type} throws {@link IllegalArgumentException}) and
 * tolerant on fields (absent fields degrade to null/zero, per {@link ControlFrame}).
 */
public final class Control {

    /** The plugin-message channel both ends register (spec §12). */
    public static final String CHANNEL = "felis:control";

    private Control() {
    }

    /**
     * encode renders {@code frame} as the channel's raw UTF-8 JSON bytes. Only the
     * fields its {@code type} defines are written, so a round-trip through
     * {@link #decode(byte[])} reproduces an equal frame.
     */
    public static byte[] encode(ControlFrame frame) {
        StringBuilder sb = new StringBuilder(96);
        sb.append("{\"type\":");
        jsonString(sb, frame.type());
        switch (frame.type()) {
            case ControlFrame.WAKE_REQUEST:
            case ControlFrame.CLAIM_REQUEST:
            case ControlFrame.TRANSFER_READY:
                kv(sb, "player", frame.player());
                kv(sb, "server", frame.server());
                break;
            case ControlFrame.STATUS_QUERY:
                kv(sb, "server", frame.server());
                break;
            case ControlFrame.STATUS_UPDATE:
                kv(sb, "server", frame.server());
                kv(sb, "phase", frame.phase());
                kvBool(sb, "ready", frame.ready());
                kvInt(sb, "playersOnline", frame.playersOnline());
                kvInt(sb, "playersMax", frame.playersMax());
                kvBool(sb, "claimable", frame.claimable());
                break;
            case ControlFrame.ERROR:
                kv(sb, "code", frame.code());
                kv(sb, "message", frame.message());
                // server is optional on Error: only emit it when the refusal is
                // server-scoped, so a bare Error frame stays minimal.
                if (frame.server() != null) {
                    kv(sb, "server", frame.server());
                }
                break;
            case ControlFrame.LIST_REQUEST:
                break;
            case ControlFrame.LIST_UPDATE:
                sb.append(",\"servers\":[");
                List<String> names = frame.servers();
                for (int i = 0; i < names.size(); i++) {
                    if (i > 0) {
                        sb.append(',');
                    }
                    jsonString(sb, names.get(i));
                }
                sb.append(']');
                break;
            case ControlFrame.LOGIN_RELEASE:
                kv(sb, "player", frame.player());
                break;
            default:
                throw new IllegalArgumentException("control: cannot encode unknown frame type '" + frame.type() + "'");
        }
        sb.append('}');
        return sb.toString().getBytes(StandardCharsets.UTF_8);
    }

    /**
     * decode parses raw channel bytes back into a {@link ControlFrame}. A malformed
     * body, a non-object root, a missing {@code type}, or an unrecognized
     * {@code type} all throw {@link IllegalArgumentException} — the caller treats a
     * bad frame as a dropped message, never a crash.
     */
    public static ControlFrame decode(byte[] data) {
        Object root;
        try {
            root = Json.parse(new String(data, StandardCharsets.UTF_8));
        } catch (RuntimeException e) {
            throw new IllegalArgumentException("control: malformed frame", e);
        }
        if (!(root instanceof Map)) {
            throw new IllegalArgumentException("control: frame is not a JSON object");
        }
        Map<?, ?> o = (Map<?, ?>) root;
        String type = str(o, "type");
        if (type == null) {
            throw new IllegalArgumentException("control: frame missing 'type'");
        }
        switch (type) {
            case ControlFrame.WAKE_REQUEST:
                return ControlFrame.wakeRequest(str(o, "player"), str(o, "server"));
            case ControlFrame.CLAIM_REQUEST:
                return ControlFrame.claimRequest(str(o, "player"), str(o, "server"));
            case ControlFrame.STATUS_QUERY:
                return ControlFrame.statusQuery(str(o, "server"));
            case ControlFrame.STATUS_UPDATE:
                return ControlFrame.statusUpdate(str(o, "server"), str(o, "phase"), bool(o, "ready"),
                        intval(o, "playersOnline"), intval(o, "playersMax"), bool(o, "claimable"));
            case ControlFrame.TRANSFER_READY:
                return ControlFrame.transferReady(str(o, "player"), str(o, "server"));
            case ControlFrame.ERROR:
                return ControlFrame.error(str(o, "code"), str(o, "message"), str(o, "server"));
            case ControlFrame.LIST_REQUEST:
                return ControlFrame.listRequest();
            case ControlFrame.LIST_UPDATE:
                return ControlFrame.listUpdate(strList(o, "servers"));
            case ControlFrame.LOGIN_RELEASE:
                return ControlFrame.loginRelease(str(o, "player"));
            default:
                throw new IllegalArgumentException("control: unknown frame type '" + type + "'");
        }
    }

    // ---- JSON writer (every field after "type" is preceded by a comma) ----

    private static void kv(StringBuilder sb, String key, String value) {
        sb.append(",\"").append(key).append("\":");
        if (value == null) {
            sb.append("null");
        } else {
            jsonString(sb, value);
        }
    }

    private static void kvBool(StringBuilder sb, String key, boolean value) {
        sb.append(",\"").append(key).append("\":").append(value);
    }

    private static void kvInt(StringBuilder sb, String key, int value) {
        sb.append(",\"").append(key).append("\":").append(value);
    }

    private static void jsonString(StringBuilder sb, String s) {
        sb.append('"');
        for (int i = 0; i < s.length(); i++) {
            char c = s.charAt(i);
            switch (c) {
                case '"':
                    sb.append("\\\"");
                    break;
                case '\\':
                    sb.append("\\\\");
                    break;
                case '\n':
                    sb.append("\\n");
                    break;
                case '\r':
                    sb.append("\\r");
                    break;
                case '\t':
                    sb.append("\\t");
                    break;
                case '\b':
                    sb.append("\\b");
                    break;
                case '\f':
                    sb.append("\\f");
                    break;
                default:
                    if (c < 0x20) {
                        sb.append(String.format("\\u%04x", (int) c));
                    } else {
                        sb.append(c);
                    }
            }
        }
        sb.append('"');
    }

    // ---- JSON readers (mirror ServerView's tolerant coercion) ----

    private static String str(Map<?, ?> o, String key) {
        Object v = o.get(key);
        return v instanceof String ? (String) v : null;
    }

    // strList keeps the string entries of an array field and skips anything else, so a
    // partly malformed list still yields the names that are well-formed.
    private static List<String> strList(Map<?, ?> o, String key) {
        List<String> out = new ArrayList<>();
        Object v = o.get(key);
        if (v instanceof List) {
            for (Object e : (List<?>) v) {
                if (e instanceof String) {
                    out.add((String) e);
                }
            }
        }
        return out;
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
