package best.lolicon.felis.neoforge;

import best.lolicon.felis.link.LinkClient;
import best.lolicon.felis.link.LinkCode;
import best.lolicon.felis.link.LinkConfig;
import best.lolicon.felis.link.LinkConfigLoader;
import best.lolicon.felis.link.LinkException;

import com.mojang.brigadier.CommandDispatcher;
import com.mojang.brigadier.exceptions.CommandSyntaxException;
import com.mojang.logging.LogUtils;
import net.minecraft.commands.CommandSourceStack;
import net.minecraft.commands.Commands;
import net.minecraft.network.chat.Component;
import net.minecraft.server.MinecraftServer;
import net.minecraft.server.level.ServerPlayer;
import net.neoforged.bus.api.IEventBus;
import net.neoforged.bus.api.SubscribeEvent;
import net.neoforged.fml.common.Mod;
import net.neoforged.fml.loading.FMLPaths;
import net.neoforged.neoforge.common.NeoForge;
import net.neoforged.neoforge.event.RegisterCommandsEvent;
import org.slf4j.Logger;

import java.io.IOException;
import java.util.UUID;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;

/**
 * FelisNeoForgeMod is the NeoForge server-side leg of the §10 account-link flow. On
 * {@link RegisterCommandsEvent} it installs a {@code /link} command that mints a
 * one-time code from felis-api for the player's already-verified UUID. As on the
 * other loaders, the HTTP call runs on a daemon I/O thread and the reply is hopped
 * back onto the server thread; failures collapse to a generic chat line with details
 * kept to the server log. If config is missing the mod stays loaded but never
 * registers the command, so the server runs un-crippled.
 *
 * <p>NeoForge constructs the mod with the mod event bus injected; the command event
 * fires on the game bus ({@link NeoForge#EVENT_BUS}), which is what we subscribe to.
 */
@Mod("felis_link")
public final class FelisNeoForgeMod {
    private static final Logger LOGGER = LogUtils.getLogger();

    private final ExecutorService io = Executors.newSingleThreadExecutor(r -> {
        Thread t = new Thread(r, "felis-link-io");
        t.setDaemon(true);
        return t;
    });
    private LinkClient linkClient;

    public FelisNeoForgeMod(IEventBus modEventBus) {
        try {
            LinkConfig config = LinkConfigLoader.load(
                    FMLPaths.CONFIGDIR.get().resolve("felis-link.properties"));
            this.linkClient = new LinkClient(config);
            NeoForge.EVENT_BUS.register(this);
            LOGGER.info("Felis link ready; /link will be registered.");
        } catch (IOException e) {
            LOGGER.error("Felis link disabled: {}", e.getMessage());
        }
    }

    @SubscribeEvent
    public void onRegisterCommands(RegisterCommandsEvent event) {
        register(event.getDispatcher());
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
