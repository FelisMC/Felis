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

/** Forge {@code /link} command; the shared {@link ModLink} does the work. Without config the mod loads but registers nothing. */
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
