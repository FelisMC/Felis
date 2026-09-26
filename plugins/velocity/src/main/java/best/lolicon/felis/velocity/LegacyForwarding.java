package best.lolicon.felis.velocity;

import best.lolicon.felis.link.ServerView;

import java.lang.reflect.Method;
import java.util.ArrayList;
import java.util.Collection;
import java.util.List;
import java.util.Set;
import java.util.TreeSet;

/**
 * LegacyForwarding keeps the Felis-Legacy fork's per-server legacy-forwarding list, the
 * system property {@value #PROPERTY}, in step with felis-api's server list (#15). A
 * server whose CR carries {@code felis.lolicon.best/forwarding=legacy} joins the list,
 * and leaves it when the label goes. The value the proxy was started with (bootstrap's
 * {@code -D}, {@code legacy18} by default) is a floor that always stays in, so an
 * install upgraded without labelling its CRs forwards exactly as before.
 *
 * <p>The fork reads the property itself; this class only rewrites it. What a rewrite
 * achieves depends on the proxy build, which {@link #detect(ClassLoader)} tells apart:
 * <ul>
 *   <li>stock Velocity has one proxy-wide mode and never reads the property, so a
 *       marked server keeps modern forwarding;</li>
 *   <li>a fork carrying patch 0003 alone parses the property once, when the first
 *       player reaches a backend. The first rewrite runs inside ProxyInitializeEvent,
 *       which Velocity waits for before it accepts a connection, so that one lands;
 *       a later change needs a proxy restart;</li>
 *   <li>a fork carrying 0004 as well re-reads the property on every backend
 *       connection, so a change applies from the next one.</li>
 * </ul>
 */
final class LegacyForwarding {
    static final String PROPERTY = "felis.legacy-forwarding.servers";
    static final String CONNECTION_CLASS = "com.velocitypowered.proxy.connection.backend.VelocityServerConnection";

    /** Proxy is what the running Velocity build does with {@link #PROPERTY}. */
    enum Proxy {
        STOCK,
        FORK_READS_ONCE,
        FORK_LIVE
    }

    /** Update is what one {@link #apply} changed, worded for the proxy log. */
    static final class Update {
        final String value;
        final List<String> added;
        final List<String> removed;
        /** True when the running proxy forwards by the new list from the next connection. */
        final boolean effective;
        /** True when the change leaves a server forwarded other than its CR asks. */
        final boolean warning;
        final String message;

        private Update(String value, List<String> added, List<String> removed, Proxy proxy, boolean effective) {
            this.value = value;
            this.added = added;
            this.removed = removed;
            this.effective = effective;
            String change = "legacy forwarding list is now [" + value + "] (added " + added + ", removed " + removed + ")";
            if (effective) {
                this.warning = false;
                this.message = "Felis: " + change + ".";
            } else if (proxy == Proxy.FORK_READS_ONCE) {
                this.warning = true;
                this.message = "Felis: " + change + ", but this Felis-Legacy build read the list when the first player "
                        + "reached a backend and keeps it until it restarts. Restart felis-velocity to apply it.";
            } else {
                this.warning = !added.isEmpty();
                this.message = "Felis: " + change + ". This proxy is stock Velocity, which forwards every backend the "
                        + "same way and ignores the list"
                        + (added.isEmpty() ? "." : ": " + added + " keep modern forwarding until felis-velocity runs "
                        + "the Felis-Legacy fork.");
            }
        }
    }

    private final Set<String> floor;
    private final Proxy proxy;
    private Set<String> current;
    private boolean accepting;

    LegacyForwarding(String startupValue, Proxy proxy) {
        this.floor = parse(startupValue);
        this.current = floor;
        this.proxy = proxy;
    }

    /**
     * accepting marks the end of ProxyInitializeEvent: from here on a fork that reads
     * the list once may already have read it.
     */
    synchronized void accepting() {
        accepting = true;
    }

    /**
     * apply rewrites the property for a server list felis-api answered with (or the
     * saved one restored at startup), and returns what changed, or null when nothing did.
     */
    synchronized Update apply(Collection<ServerView> servers) {
        Set<String> next = new TreeSet<>(floor);
        for (ServerView v : servers) {
            if (v.legacyForwarding() && v.name() != null && !v.name().isEmpty()) {
                next.add(v.name());
            }
        }
        if (next.equals(current)) {
            return null;
        }
        List<String> added = new ArrayList<>();
        for (String n : next) {
            if (!current.contains(n)) {
                added.add(n);
            }
        }
        List<String> removed = new ArrayList<>();
        for (String n : current) {
            if (!next.contains(n)) {
                removed.add(n);
            }
        }
        String value = String.join(",", next);
        System.setProperty(PROPERTY, value);
        current = next;
        boolean effective = proxy == Proxy.FORK_LIVE || (proxy == Proxy.FORK_READS_ONCE && !accepting);
        return new Update(value, added, removed, proxy, effective);
    }

    /** parse reads the property's comma-separated form the way the fork does. */
    static Set<String> parse(String value) {
        Set<String> out = new TreeSet<>();
        if (value == null) {
            return out;
        }
        for (String s : value.split(",")) {
            String t = s.trim();
            if (!t.isEmpty()) {
                out.add(t);
            }
        }
        return out;
    }

    static Proxy detect(ClassLoader loader) {
        return detect(loader, CONNECTION_CLASS);
    }

    /**
     * detect looks for the methods the fork patches add to the backend connection class.
     * The class is loaded without being initialized: initializing it would run a 0003-only
     * fork's static parse of the property before {@link #apply} had rewritten it.
     */
    static Proxy detect(ClassLoader loader, String className) {
        boolean perServer = false;
        boolean live = false;
        try {
            Class<?> c = Class.forName(className, false, loader);
            for (Method m : c.getDeclaredMethods()) {
                perServer |= m.getName().equals("effectiveForwardingMode");
                live |= m.getName().equals("felisLegacyForwardingServers");
            }
        } catch (ClassNotFoundException | LinkageError e) {
            return Proxy.STOCK;
        }
        if (live && perServer) {
            return Proxy.FORK_LIVE;
        }
        return perServer ? Proxy.FORK_READS_ONCE : Proxy.STOCK;
    }
}
