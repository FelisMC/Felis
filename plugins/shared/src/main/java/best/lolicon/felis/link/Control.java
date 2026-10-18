package best.lolicon.felis.link;

import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;

// Use raw UTF-8 JSON; reject unknown structures while defaulting absent fields safely.
public final class Control {

    /** The plugin-message channel both ends register. */
    public static final String CHANNEL = "felis:control";

    private Control() {
    }

    /** encode renders frame as the channel's raw UTF-8 JSON bytes, writing only the fields its type defines, so a decode round-trip reproduces an equal frame. */
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
                kvList(sb, "servers", frame.servers());
                // Only a list that carries verdicts writes them, so a names-only
                // frame stays the shape it always was.
                if (!frame.access().isEmpty()) {
                    kvList(sb, "access", frame.access());
                }
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

    // Malformed, missing-type, and unknown-type frames throw IllegalArgumentException.
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
                return listUpdate(o);
            case ControlFrame.LOGIN_RELEASE:
                return ControlFrame.loginRelease(str(o, "player"));
            default:
                throw new IllegalArgumentException("control: unknown frame type '" + type + "'");
        }
    }


    private static void kv(StringBuilder sb, String key, String value) {
        sb.append(",\"").append(key).append("\":");
        if (value == null) {
            sb.append("null");
        } else {
            jsonString(sb, value);
        }
    }

    private static void kvList(StringBuilder sb, String key, List<String> values) {
        sb.append(",\"").append(key).append("\":[");
        for (int i = 0; i < values.size(); i++) {
            if (i > 0) {
                sb.append(',');
            }
            jsonString(sb, values.get(i));
        }
        sb.append(']');
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


    private static String str(Map<?, ?> o, String key) {
        Object v = o.get(key);
        return v instanceof String ? (String) v : null;
    }

    // listUpdate keeps the string entries of "servers" and skips the rest. "access" is read by the same index, so a skipped name takes its verdict with it.
    private static ControlFrame listUpdate(Map<?, ?> o) {
        List<String> names = new ArrayList<>();
        List<String> access = new ArrayList<>();
        List<?> rawNames = o.get("servers") instanceof List<?> l ? l : List.of();
        List<?> rawAccess = o.get("access") instanceof List<?> l ? l : List.of();
        for (int i = 0; i < rawNames.size(); i++) {
            if (rawNames.get(i) instanceof String name) {
                names.add(name);
                access.add(i < rawAccess.size() && rawAccess.get(i) instanceof String v ? v : "");
            }
        }
        return ControlFrame.listUpdate(names, access);
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
