package best.lolicon.felis.link;

import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * Json is a minimal, dependency-free JSON reader. It exists so the shared link
 * core stays zero-dependency: the {@code srcDir}-sharing build model compiles
 * this source straight into every platform jar, so pulling in Gson/Jackson would
 * force shading the parser into four loaders. It parses the small, well-formed
 * bodies the felis-api internal face returns — a flat success object, or the
 * nested {@code {"error":{"code","message"}}} envelope — and nothing more exotic
 * is required.
 *
 * <p>The parser is package-private and intentionally strict: callers wrap a parse
 * failure as a "bad response from felis-api" condition rather than guessing.
 */
final class Json {
    private final String s;
    private int i;

    private Json(String s) {
        this.s = s;
    }

    /**
     * quote renders s as a JSON string literal, for the few request bodies that carry
     * text a player typed. Quotes, backslashes and control characters are escaped.
     */
    static String quote(String s) {
        StringBuilder b = new StringBuilder(s.length() + 2).append('"');
        for (int k = 0; k < s.length(); k++) {
            char c = s.charAt(k);
            switch (c) {
                case '"':
                    b.append("\\\"");
                    break;
                case '\\':
                    b.append("\\\\");
                    break;
                default:
                    if (c < 0x20) {
                        b.append(String.format("\\u%04x", (int) c));
                    } else {
                        b.append(c);
                    }
            }
        }
        return b.append('"').toString();
    }

    /** parse reads a single JSON value from text, rejecting trailing garbage. */
    static Object parse(String text) {
        Json p = new Json(text);
        p.ws();
        Object v = p.value();
        p.ws();
        if (p.i < p.s.length()) {
            throw p.err("trailing content");
        }
        return v;
    }

    private Object value() {
        if (i >= s.length()) {
            throw err("unexpected end of input");
        }
        char c = s.charAt(i);
        switch (c) {
            case '{':
                return object();
            case '[':
                return array();
            case '"':
                return string();
            case 't':
            case 'f':
                return bool();
            case 'n':
                return nul();
            default:
                return number();
        }
    }

    private Map<String, Object> object() {
        Map<String, Object> m = new LinkedHashMap<>();
        expect('{');
        ws();
        if (peek() == '}') {
            i++;
            return m;
        }
        while (true) {
            ws();
            String key = string();
            ws();
            expect(':');
            ws();
            m.put(key, value());
            ws();
            char c = next();
            if (c == '}') {
                return m;
            }
            if (c != ',') {
                throw err("expected ',' or '}' in object");
            }
        }
    }

    private List<Object> array() {
        List<Object> l = new ArrayList<>();
        expect('[');
        ws();
        if (peek() == ']') {
            i++;
            return l;
        }
        while (true) {
            ws();
            l.add(value());
            ws();
            char c = next();
            if (c == ']') {
                return l;
            }
            if (c != ',') {
                throw err("expected ',' or ']' in array");
            }
        }
    }

    private String string() {
        expect('"');
        StringBuilder sb = new StringBuilder();
        while (true) {
            if (i >= s.length()) {
                throw err("unterminated string");
            }
            char c = s.charAt(i++);
            if (c == '"') {
                return sb.toString();
            }
            if (c == '\\') {
                if (i >= s.length()) {
                    throw err("unterminated escape");
                }
                char e = s.charAt(i++);
                switch (e) {
                    case '"':
                        sb.append('"');
                        break;
                    case '\\':
                        sb.append('\\');
                        break;
                    case '/':
                        sb.append('/');
                        break;
                    case 'b':
                        sb.append('\b');
                        break;
                    case 'f':
                        sb.append('\f');
                        break;
                    case 'n':
                        sb.append('\n');
                        break;
                    case 'r':
                        sb.append('\r');
                        break;
                    case 't':
                        sb.append('\t');
                        break;
                    case 'u':
                        if (i + 4 > s.length()) {
                            throw err("truncated unicode escape");
                        }
                        sb.append((char) Integer.parseInt(s.substring(i, i + 4), 16));
                        i += 4;
                        break;
                    default:
                        throw err("invalid escape '\\" + e + "'");
                }
            } else {
                sb.append(c);
            }
        }
    }

    private Object number() {
        int start = i;
        while (i < s.length() && "+-0123456789.eE".indexOf(s.charAt(i)) >= 0) {
            i++;
        }
        String num = s.substring(start, i);
        if (num.isEmpty()) {
            throw err("invalid value");
        }
        return Double.parseDouble(num);
    }

    private Boolean bool() {
        if (s.startsWith("true", i)) {
            i += 4;
            return Boolean.TRUE;
        }
        if (s.startsWith("false", i)) {
            i += 5;
            return Boolean.FALSE;
        }
        throw err("invalid literal");
    }

    private Object nul() {
        if (s.startsWith("null", i)) {
            i += 4;
            return null;
        }
        throw err("invalid literal");
    }

    private void ws() {
        while (i < s.length()) {
            char c = s.charAt(i);
            if (c == ' ' || c == '\t' || c == '\n' || c == '\r') {
                i++;
            } else {
                break;
            }
        }
    }

    private char peek() {
        return i < s.length() ? s.charAt(i) : '\0';
    }

    private char next() {
        return i < s.length() ? s.charAt(i++) : '\0';
    }

    private void expect(char c) {
        if (i >= s.length() || s.charAt(i) != c) {
            throw err("expected '" + c + "'");
        }
        i++;
    }

    private IllegalArgumentException err(String msg) {
        return new IllegalArgumentException("json: " + msg + " at index " + i);
    }
}
