package best.lolicon.felis.velocity;

import best.lolicon.felis.link.ServerView;

import com.velocitypowered.api.event.Subscribe;
import com.velocitypowered.api.event.proxy.ProxyPingEvent;
import com.velocitypowered.api.proxy.server.ServerPing;
import net.kyori.adventure.text.Component;
import net.kyori.adventure.text.format.NamedTextColor;

import java.net.InetSocketAddress;
import java.util.Optional;

/**
 * MotdResponder answers the server-list ping for a felis subdomain from cache, so
 * a player sees the right server in their list — and whether it is awake — without
 * the ping itself waking anything (spec §11 "ping MOTD：只读缓存，后台刷新"). The
 * cache it reads is the {@link ServerRegistry}'s lifecycle view, refreshed in the
 * background by the plugin's registration poll; the ping handler never touches the
 * backend or the wake lever.
 *
 * <p>This is the read-only, phase-aware subset of the responsibility: the MOTD is
 * synthesized from the server's lifecycle (online / starting / sleeping) and its
 * cached player counts. Mirroring each backend's <em>own</em> MOTD string (by
 * pinging ready servers in the background and caching the result) is a richer
 * variant deferred to a later slice; nothing here ever pings a sleeping backend.
 */
public final class MotdResponder {
    private final ServerRegistry registry;

    MotdResponder(ServerRegistry registry) {
        this.registry = registry;
    }

    @Subscribe
    public void onProxyPing(ProxyPingEvent event) {
        Optional<InetSocketAddress> vh = event.getConnection().getVirtualHost();
        if (vh.isEmpty()) {
            return; // no SRV host → leave the proxy's own MOTD
        }
        Optional<ServerView> viewOpt = registry.resolveByHost(vh.get().getHostString());
        if (viewOpt.isEmpty()) {
            return; // not a felis subdomain → leave the proxy's own MOTD
        }
        ServerView v = viewOpt.get();

        ServerPing.Builder b = event.getPing().asBuilder();
        b.description(Component.text("« " + v.name() + " » ", NamedTextColor.AQUA)
                .append(Component.text(statusLine(v), statusColor(v))));
        if (v.ready()) {
            b.onlinePlayers(v.playersOnline());
            b.maximumPlayers(Math.max(v.playersMax(), v.playersOnline()));
        }
        event.setPing(b.build());
    }

    // The server-list ping carries no client locale, so the MOTD status uses the
    // both-languages-in-one-line pattern the modded /link clients share.
    private static String statusLine(ServerView v) {
        if (v.ready()) {
            return "在线 / online";
        }
        if ("Running".equals(v.desiredState())) {
            return "启动中… / starting…";
        }
        return "休眠中，加入即唤醒 / sleeping — join to wake";
    }

    private static NamedTextColor statusColor(ServerView v) {
        if (v.ready()) {
            return NamedTextColor.GREEN;
        }
        if ("Running".equals(v.desiredState())) {
            return NamedTextColor.YELLOW;
        }
        return NamedTextColor.GRAY;
    }
}
