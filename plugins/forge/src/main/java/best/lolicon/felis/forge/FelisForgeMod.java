package best.lolicon.felis.forge;

import best.lolicon.felis.link.LinkClient;
import best.lolicon.felis.link.LinkConfig;
import best.lolicon.felis.link.LinkConfigLoader;
import best.lolicon.felis.link.ModLink;

import com.mojang.brigadier.CommandDispatcher;
import com.mojang.brigadier.exceptions.CommandSyntaxException;
import com.mojang.logging.LogUtils;
import net.minecraft.commands.CommandSourceStack;
import net.minecraft.commands.Commands;
import net.minecraft.network.chat.Component;
import net.minecraft.server.level.ServerPlayer;
import net.minecraftforge.common.MinecraftForge;
import net.minecraftforge.event.RegisterCommandsEvent;
import net.minecraftforge.eventbus.api.SubscribeEvent;
import net.minecraftforge.fml.common.Mod;
import net.minecraftforge.fml.loading.FMLPaths;
import org.slf4j.Logger;

import java.io.IOException;
import java.util.UUID;
import java.util.concurrent.Executor;
import java.util.concurrent.Executors;

/**
 * FelisForgeMod is the Forge server-side leg of the §10 account-link flow. On
 * {@link RegisterCommandsEvent} it installs a {@code /link} command that mints a
 * one-time code from felis-api for the player's already-verified UUID. The command
 * is the shared {@link ModLink}, as on the other loaders (the HTTP call on a daemon
 * I/O thread, the reply back on the server thread, one generic line for any
 * failure). If config is missing the mod stays loaded but
 * never registers the command, so the proxy/server runs un-crippled.
 */
@Mod("felis_link")
public final class FelisForgeMod {
    private static final Logger LOGGER = LogUtils.getLogger();

    private final Executor io = Executors.newSingleThreadExecutor(r -> {
        Thread t = new Thread(r, "felis-link-io");
        t.setDaemon(true);
        return t;
    });
    private ModLink link;

    public FelisForgeMod() {
        try {
            LinkConfig config = LinkConfigLoader.load(
                    FMLPaths.CONFIGDIR.get().resolve("felis-link.properties"));
            this.link = new ModLink(new LinkClient(config), io, LOGGER::warn);
            MinecraftForge.EVENT_BUS.register(this);
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
        dispatcher.register(Commands.literal("link").executes(ctx -> link.run(new LinkSource(ctx.getSource()))));
    }

    /** LinkSource is a command source as the shared /link sees it. */
    private static final class LinkSource implements ModLink.Source {
        private final CommandSourceStack source;
        private final ServerPlayer player; // null for the console or a command block

        LinkSource(CommandSourceStack source) {
            this.source = source;
            ServerPlayer p;
            try {
                p = source.getPlayerOrException();
            } catch (CommandSyntaxException e) {
                p = null;
            }
            this.player = p;
        }

        @Override
        public UUID player() {
            return player != null ? player.getUUID() : null;
        }

        @Override
        public boolean onlineMode() {
            return source.getServer().usesAuthentication();
        }

        @Override
        public void fail(String line) {
            source.sendFailure(Component.literal(line));
        }

        @Override
        public void tell(String line) {
            player.sendSystemMessage(Component.literal(line));
        }

        @Override
        public void onServerThread(Runnable task) {
            source.getServer().execute(task);
        }
    }
}
