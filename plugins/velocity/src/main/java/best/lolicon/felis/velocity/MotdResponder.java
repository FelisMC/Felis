package best.lolicon.felis.velocity;

import best.lolicon.felis.link.ServerView;

import com.velocitypowered.api.event.Subscribe;
import com.velocitypowered.api.event.proxy.ProxyPingEvent;
import com.velocitypowered.api.proxy.server.ServerPing;
import net.kyori.adventure.text.Component;
import net.kyori.adventure.text.format.NamedTextColor;

import java.net.InetSocketAddress;
import java.util.Optional;

// Answer pings from cached lifecycle data without contacting or waking sleeping backends.
public final class MotdResponder {
    private static final String PHASE_FAILED = "Failed";
    private static final String DESIRED_HIBERNATED = "Hibernated";

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

    // Pings have no locale; prioritize start failures over desiredState Running when composing MOTD.
    static String statusLine(ServerView v) {
        if (v.ready()) {
            return "在线 / online";
        }
        if (v.startGaveUp()) {
            return "启动失败，等服主处理 / failed to start — the owner has to restart it";
        }
        if (PHASE_FAILED.equals(v.phase())) {
            return "启动超时，稍后自动重试 / start timed out — retrying shortly";
        }
        if ("Running".equals(v.desiredState())) {
            return "启动中… / starting…";
        }
        // The ping carries no identity either, so a server that is down says who can wake
        // it, by the same autostartPolicy gate the join runs into: ownerOnly or unset
        // admits the owner alone.
        boolean frozen = DESIRED_HIBERNATED.equals(v.desiredState());
        String zh = frozen ? "已休眠" : "已停止";
        String en = frozen ? "hibernated" : "stopped";
        String zhWake = frozen ? "唤醒" : "启动";
        String enWake = frozen ? "resume" : "start";
        if ("public".equals(v.autostartPolicy())) {
            return zh + "，加入即" + zhWake + " / " + en + " — join to " + enWake;
        }
        if ("allowlist".equals(v.autostartPolicy())) {
            return zh + "，名单内玩家加入即" + zhWake + " / " + en + " — allowlisted players can " + enWake + " it";
        }
        return zh + "，仅服主可" + zhWake + " / " + en + " — only the owner can " + enWake + " it";
    }

    static NamedTextColor statusColor(ServerView v) {
        if (v.ready()) {
            return NamedTextColor.GREEN;
        }
        if (v.startGaveUp()) {
            return NamedTextColor.RED;
        }
        if ("Running".equals(v.desiredState())) {
            return NamedTextColor.YELLOW;
        }
        return NamedTextColor.GRAY;
    }
}
