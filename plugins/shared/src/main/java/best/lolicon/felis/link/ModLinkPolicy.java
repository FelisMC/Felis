package best.lolicon.felis.link;

/**
 * ModLinkPolicy is what the Fabric, Forge and NeoForge mods say when they refuse
 * {@code /link}. A mod mints a link code for the UUID its server reports, and only an
 * online-mode server has checked that UUID with Mojang. Behind a proxy the backend runs
 * offline-mode and the proxy's own {@code /link} already serves every backend; on a
 * cracked server the UUID is whatever the client claims, so a code minted there would
 * let anyone link someone else's Minecraft account. The mods therefore link only on a
 * standalone online-mode server.
 */
public final class ModLinkPolicy {
    /** OFFLINE_REPLY is the chat line a player gets on an offline-mode server. */
    public static final String OFFLINE_REPLY =
            "此服务器未开启正版验证，不能在这里绑定 / This server runs with online-mode off, so /link is unavailable here.";

    /** OFFLINE_LOG is the server-log line for the same refusal. */
    public static final String OFFLINE_LOG =
            "Felis link: refused /link because online-mode is off. The mod links only on a standalone "
                    + "online-mode server; behind the Felis proxy, the proxy serves /link.";

    private ModLinkPolicy() {
    }
}
