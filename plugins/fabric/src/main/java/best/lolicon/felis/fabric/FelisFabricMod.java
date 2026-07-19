package best.lolicon.felis.fabric;

import best.lolicon.felis.link.LinkClient;
import best.lolicon.felis.link.LinkCode;
import best.lolicon.felis.link.LinkConfig;
import best.lolicon.felis.link.LinkConfigLoader;
import best.lolicon.felis.link.LinkException;

import com.mojang.brigadier.CommandDispatcher;
import com.mojang.brigadier.exceptions.CommandSyntaxException;
import com.mojang.logging.LogUtils;
import net.fabricmc.api.DedicatedServerModInitializer;
import net.fabricmc.fabric.api.command.v2.CommandRegistrationCallback;
import net.fabricmc.loader.api.FabricLoader;
import net.minecraft.commands.CommandSourceStack;
import net.minecraft.commands.Commands;
import net.minecraft.network.chat.Component;
import net.minecraft.server.MinecraftServer;
import net.minecraft.server.level.ServerPlayer;
import org.slf4j.Logger;

import java.io.IOException;
import java.util.UUID;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;

/**
 * FelisFabricMod is the Fabric (dedicated-server) leg of the §10 account-link
 * flow. A server-side {@code /link} command takes the player's already-verified
 * UUID, asks felis-api for a one-time code, and shows it in chat; the player then
 * redeems it on the web console. The HTTP call is pushed onto a daemon I/O thread
 * and the reply is hopped back onto the server thread, so a slow felis-api never
 * stalls the tick loop. Failures collapse to a generic chat line with details
 * confined to the server log.
 */
public final class FelisFabricMod implements DedicatedServerModInitializer {
    private static final Logger LOGGER = LogUtils.getLogger();

    private final ExecutorService io = Executors.newSingleThreadExecutor(r -> {
        Thread t = new Thread(r, "felis-link-io");
        t.setDaemon(true);
        return t;
    });
    private LinkClient linkClient;

    @Override
    public void onInitializeServer() {
        try {
            LinkConfig config = LinkConfigLoader.load(
                    FabricLoader.getInstance().getConfigDir().resolve("felis-link.properties"));
            this.linkClient = new LinkClient(config);
        } catch (IOException e) {
            LOGGER.error("Felis link disabled: {}", e.getMessage());
            return;
        }
        CommandRegistrationCallback.EVENT.register(
                (dispatcher, registry, environment) -> register(dispatcher));
        LOGGER.info("Felis link ready; /link is registered.");
    }

    private void register(CommandDispatcher<CommandSourceStack> dispatcher) {
        dispatcher.register(Commands.literal("link").executes(ctx -> {
            CommandSourceStack source = ctx.getSource();
            ServerPlayer player;
            try {
                player = source.getPlayerOrException();
            } catch (CommandSyntaxException e) {
                source.sendFailure(Component.literal("/link 只能由玩家执行 / /link can only be run by a player."));
                return 0;
            }
            requestAndReply(source.getServer(), player);
            return 1;
        }));
    }

    private void requestAndReply(MinecraftServer server, ServerPlayer player) {
        UUID uuid = player.getUUID();
        player.sendSystemMessage(Component.literal("正在获取绑定码… / Requesting a link code…"));
        io.submit(() -> {
            try {
                LinkCode code = linkClient.requestCode(uuid);
                server.execute(() -> {
                    player.sendSystemMessage(Component.literal(
                            "绑定码 / Link code: " + code.code() + "（几分钟内有效 / valid a few minutes）"));
                    player.sendSystemMessage(Component.literal(code.panelUrl() != null
                            ? "在此完成绑定 / Finish linking at: " + code.panelUrl()
                            : "在网页控制台 → 账户 中输入 / Enter it on the web console → Account."));
                });
            } catch (LinkException e) {
                LOGGER.warn("link code request failed for {} (status={}, code={}): {}",
                        uuid, e.statusCode(), e.errorCode(), e.getMessage());
                server.execute(() -> player.sendSystemMessage(Component.literal(
                        "现在无法获取绑定码，请稍后再试 / Couldn't get a link code right now. Please try again in a moment.")));
            }
        });
    }
}
