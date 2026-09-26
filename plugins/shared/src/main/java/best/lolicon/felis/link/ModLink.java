package best.lolicon.felis.link;

import java.util.UUID;
import java.util.concurrent.Executor;
import java.util.function.Consumer;

/**
 * ModLink is the {@code /link} command the Fabric, Forge and NeoForge mods share (the
 * §10 account-link flow): it takes the player's server-verified UUID, asks felis-api
 * for a one-time code off the server thread, and shows the code and where to redeem
 * it back on the server thread, so a slow felis-api never stalls the tick loop. A
 * failure reaches the player as one generic line, with the details in the server log.
 * Each mod only adapts its command source to {@link Source}; ModLinkTest drives this
 * class against a stub felis-api.
 *
 * <p>A mod mints a link code for the UUID its server reports, and only an online-mode
 * server has checked that UUID with Mojang. Behind a proxy the backend runs
 * offline-mode and the proxy's own {@code /link} already serves every backend; on a
 * cracked server the UUID is whatever the client claims, so a code minted there would
 * let anyone link someone else's Minecraft account. The mods therefore link only on a
 * standalone online-mode server.
 */
public final class ModLink {
    /** PLAYERS_ONLY answers /link from the console or a command block. */
    public static final String PLAYERS_ONLY = "/link 只能由玩家执行 / /link can only be run by a player.";

    /** OFFLINE_REPLY is the chat line a player gets on an offline-mode server. */
    public static final String OFFLINE_REPLY =
            "此服务器未开启正版验证，不能在这里绑定 / This server runs with online-mode off, so /link is unavailable here.";

    /** OFFLINE_LOG is the server-log line for the same refusal. */
    public static final String OFFLINE_LOG =
            "Felis link: refused /link because online-mode is off. The mod links only on a standalone "
                    + "online-mode server; behind the Felis proxy, the proxy serves /link.";

    /** REQUESTING is said at once, while the code is being fetched. */
    public static final String REQUESTING = "正在获取绑定码… / Requesting a link code…";

    /** FAILED is the one line a player gets for any failed fetch. */
    public static final String FAILED =
            "现在无法获取绑定码，请稍后再试 / Couldn't get a link code right now. Please try again in a moment.";

    /** CONSOLE_HINT says where to redeem a code that came without a panel URL. */
    public static final String CONSOLE_HINT = "在网页控制台 → 账户 中输入 / Enter it on the web console → Account.";

    /** Source is the command's caller as a mod sees it. */
    public interface Source {
        /** player is the UUID of the player running /link, or null for the console or a command block. */
        UUID player();

        /** onlineMode reports whether this server checks players with Mojang. */
        boolean onlineMode();

        /** fail answers the command with a failure line. */
        void fail(String line);

        /** tell sends the player a chat line; it is called on the server thread. */
        void tell(String line);

        /** onServerThread runs a task on the server thread. */
        void onServerThread(Runnable task);
    }

    private final LinkClient client;
    private final Executor io;
    private final Consumer<String> warn;

    /**
     * @param io   runs the felis-api call; the mods pass one daemon thread
     * @param warn writes a warning to the server log
     */
    public ModLink(LinkClient client, Executor io, Consumer<String> warn) {
        this.client = client;
        this.io = io;
        this.warn = warn;
    }

    /** run executes /link for one caller and returns the command result: 1 ran, 0 refused. */
    public int run(Source source) {
        UUID id = source.player();
        if (id == null) {
            source.fail(PLAYERS_ONLY);
            return 0;
        }
        if (!source.onlineMode()) {
            warn.accept(OFFLINE_LOG);
            source.fail(OFFLINE_REPLY);
            return 0;
        }
        source.tell(REQUESTING);
        io.execute(() -> {
            try {
                LinkCode code = client.requestCode(id);
                source.onServerThread(() -> {
                    source.tell(codeLine(code));
                    source.tell(code.panelUrl() != null ? "在此完成绑定 / Finish linking at: " + code.panelUrl() : CONSOLE_HINT);
                });
            } catch (LinkException e) {
                warn.accept("Felis link: link code request failed for " + id + " (status=" + e.statusCode()
                        + ", code=" + e.errorCode() + "): " + e.getMessage());
                source.onServerThread(() -> source.tell(FAILED));
            }
        });
        return 1;
    }

    static String codeLine(LinkCode code) {
        return "绑定码 / Link code: " + code.code() + "（几分钟内有效 / valid a few minutes）";
    }
}
