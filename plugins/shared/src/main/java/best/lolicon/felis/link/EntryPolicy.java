package best.lolicon.felis.link;

import java.util.Map;

/** Owner-selected entry routing; authentication remains the proxy's responsibility. */
public record EntryPolicy(String mode, String defaultServer, boolean requireAccountLink,
                          String offlineAction, String waitingSpace, String fallbackServer) {
    public static EntryPolicy legacy() {
        return new EntryPolicy("domain", "", true, "wake", "lobby", "");
    }

    public static EntryPolicy fromJson(Map<?, ?> data) throws LinkException {
        String mode = text(data, "mode");
        String action = text(data, "offlineAction");
        String space = text(data, "waitingSpace");
        if (!(mode.equals("lobby") || mode.equals("direct") || mode.equals("domain"))
                || !(action.equals("wake") || action.equals("fallback") || action.equals("disconnect"))
                || !(space.equals("login") || space.equals("lobby"))
                || !(data.get("requireAccountLink") instanceof Boolean)) {
            throw new LinkException(503, "entry_policy_unavailable", "Invalid player entry policy");
        }
        return new EntryPolicy(mode, text(data, "defaultServer"), (boolean) data.get("requireAccountLink"),
                action, space, text(data, "fallbackServer"));
    }

    private static String text(Map<?, ?> data, String key) {
        return data.get(key) instanceof String value ? value : "";
    }
}
