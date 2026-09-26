package best.lolicon.felis.fabric;

import best.lolicon.felis.link.LinkClient;
import best.lolicon.felis.link.LinkConfig;
import best.lolicon.felis.link.LinkConfigLoader;
import best.lolicon.felis.link.ModLink;

import com.mojang.brigadier.CommandDispatcher;
import com.mojang.brigadier.exceptions.CommandSyntaxException;
import com.mojang.logging.LogUtils;
import net.fabricmc.api.DedicatedServerModInitializer;
import net.fabricmc.fabric.api.command.v2.CommandRegistrationCallback;
import net.fabricmc.loader.api.FabricLoader;
import net.minecraft.commands.CommandSourceStack;
import net.minecraft.commands.Commands;
import net.minecraft.network.chat.Component;
import net.minecraft.server.level.ServerPlayer;
import org.slf4j.Logger;

import java.io.IOException;
import java.util.UUID;
import java.util.concurrent.Executor;
import java.util.concurrent.Executors;

/**
 * FelisFabricMod is the Fabric (dedicated-server) leg of the §10 account-link
 * flow. A server-side {@code /link} command takes the player's already-verified
 * UUID, asks felis-api for a one-time code, and shows it in chat; the player then
 * redeems it on the web console. The command itself is the shared {@link ModLink}
 * (the HTTP call on a daemon I/O thread, the reply back on the server thread, one
 * generic line for any failure); this class registers it and adapts the command
 * source.
 */
public final class FelisFabricMod implements DedicatedServerModInitializer {
    private static final Logger LOGGER = LogUtils.getLogger();

    private final Executor io = Executors.newSingleThreadExecutor(r -> {
        Thread t = new Thread(r, "felis-link-io");
        t.setDaemon(true);
        return t;
    });
    private ModLink link;

    @Override
    public void onInitializeServer() {
        try {
            LinkConfig config = LinkConfigLoader.load(
                    FabricLoader.getInstance().getConfigDir().resolve("felis-link.properties"));
            this.link = new ModLink(new LinkClient(config), io, LOGGER::warn);
        } catch (IOException e) {
            LOGGER.error("Felis link disabled: {}", e.getMessage());
            return;
        }
        CommandRegistrationCallback.EVENT.register(
                (dispatcher, registry, environment) -> register(dispatcher));
        LOGGER.info("Felis link ready; /link is registered.");
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
