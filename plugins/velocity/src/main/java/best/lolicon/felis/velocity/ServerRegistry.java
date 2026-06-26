package best.lolicon.felis.velocity;

import best.lolicon.felis.link.ServerView;

import com.velocitypowered.api.proxy.ProxyServer;
import com.velocitypowered.api.proxy.server.RegisteredServer;
import com.velocitypowered.api.proxy.server.ServerInfo;
import org.slf4j.Logger;

import java.net.InetSocketAddress;
import java.util.ArrayList;
import java.util.Collection;
import java.util.HashSet;
import java.util.Locale;
import java.util.Map;
import java.util.Optional;
import java.util.Set;
import java.util.concurrent.ConcurrentHashMap;

/**
 * ServerRegistry mirrors the felis server set into Velocity's dynamic server
 * registry and indexes it for host-based routing (spec §11). It owns two things:
 * the felis lifecycle views keyed by name + subdomain, and the registration of
 * each server's backend address as a Velocity {@link RegisteredServer}.
 *
 * <p>{@link #refresh(Collection)} is called only on a <em>successful</em> fetch
 * from felis-api, so a name's absence is a genuine removal. On an API failure the
 * caller skips the refresh entirely and the previous registrations survive
 * untouched (spec §11 keep-old-on-failure) — a transient control-plane blip must
 * never deregister live backends out from under connected players.
 *
 * <p>Only felis-managed servers live in this registry; servers defined statically
 * in {@code velocity.toml} (notably the lobby) are never added here and so are
 * never deregistered by a refresh. Static server names must therefore not collide
 * with felis server names.
 */
final class ServerRegistry {
    private static final int DEFAULT_PORT = 25565;

    private final ProxyServer proxy;
    private final Logger log;
    private final String rootDomain;

    private final Map<String, ServerView> byName = new ConcurrentHashMap<>();
    private final Map<String, String> subdomainToName = new ConcurrentHashMap<>();

    ServerRegistry(ProxyServer proxy, Logger log, String rootDomain) {
        this.proxy = proxy;
        this.log = log;
        this.rootDomain = rootDomain.toLowerCase(Locale.ROOT);
    }

    /** refresh reconciles registrations against a freshly fetched server list. */
    void refresh(Collection<ServerView> servers) {
        Set<String> seen = new HashSet<>();
        for (ServerView v : servers) {
            String name = v.name();
            if (name == null || name.isEmpty()) {
                continue;
            }
            seen.add(name);
            byName.put(name, v);
            String sub = v.subdomain();
            if (sub != null && !sub.isEmpty()) {
                subdomainToName.put(sub.toLowerCase(Locale.ROOT), name);
            }
            ensureRegistered(v);
        }
        // Drop servers that vanished from a successful fetch (iterate a snapshot so
        // deregister can mutate byName underneath us).
        for (String name : new ArrayList<>(byName.keySet())) {
            if (!seen.contains(name)) {
                deregister(name);
            }
        }
        subdomainToName.values().removeIf(n -> !seen.contains(n));
    }

    private void ensureRegistered(ServerView v) {
        String addr = v.endpointAddress();
        if (addr == null || addr.isEmpty()) {
            return; // no backend address yet (server never started) → nothing to register
        }
        InetSocketAddress target = parseAddress(addr);
        Optional<RegisteredServer> existing = proxy.getServer(v.name());
        if (existing.isPresent()) {
            if (existing.get().getServerInfo().getAddress().equals(target)) {
                return; // already registered at this address
            }
            proxy.unregisterServer(existing.get().getServerInfo()); // address changed → re-register
        }
        proxy.registerServer(new ServerInfo(v.name(), target));
        log.info("Felis: registered backend {} -> {}", v.name(), addr);
    }

    private void deregister(String name) {
        byName.remove(name);
        proxy.getServer(name).ifPresent(rs -> {
            proxy.unregisterServer(rs.getServerInfo());
            log.info("Felis: deregistered backend {}", name);
        });
    }

    /** resolveByHost maps {@code subdomain.<root_domain>} to its current view. */
    Optional<ServerView> resolveByHost(String host) {
        if (host == null) {
            return Optional.empty();
        }
        String h = host.toLowerCase(Locale.ROOT);
        String suffix = "." + rootDomain;
        if (!h.endsWith(suffix)) {
            return Optional.empty();
        }
        String sub = h.substring(0, h.length() - suffix.length());
        String name = subdomainToName.get(sub);
        return name == null ? Optional.empty() : Optional.ofNullable(byName.get(name));
    }

    ServerView view(String name) {
        return name == null ? null : byName.get(name);
    }

    boolean isManaged(String name) {
        return name != null && byName.containsKey(name);
    }

    Optional<RegisteredServer> registered(String name) {
        return proxy.getServer(name);
    }

    Collection<ServerView> all() {
        return new ArrayList<>(byName.values());
    }

    /** parseAddress splits {@code host[:port]} into an unresolved socket address. */
    static InetSocketAddress parseAddress(String addr) {
        int idx = addr.lastIndexOf(':');
        if (idx > 0 && idx < addr.length() - 1) {
            try {
                int port = Integer.parseInt(addr.substring(idx + 1));
                // Unresolved: the backend's DNS (a K8s Service) may not resolve yet
                // while the server is asleep; Velocity resolves at connect time.
                return InetSocketAddress.createUnresolved(addr.substring(0, idx), port);
            } catch (NumberFormatException ignored) {
                // not host:port → fall through to the default Minecraft port
            }
        }
        return InetSocketAddress.createUnresolved(addr, DEFAULT_PORT);
    }
}
