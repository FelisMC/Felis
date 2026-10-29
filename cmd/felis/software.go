package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"felis.lolicon.best/internal/artifactcache"
	"felis.lolicon.best/internal/software"
)

// Environment the operator gives a runtime-image server's containers (mirrors
// internal/operator).
const (
	softwareEnv      = "FELIS_SOFTWARE"
	artifactCacheEnv = "FELIS_ARTIFACT_CACHE"
	// gamePort is the port every server listens on behind the proxy (mirrors
	// internal/operator.GamePort).
	gamePort = 25565
)

// readSoftwareSpec decodes the spec the operator passes in FELIS_SOFTWARE.
func readSoftwareSpec() (software.Spec, error) {
	raw := os.Getenv(softwareEnv)
	if raw == "" {
		return software.Spec{}, fmt.Errorf("%s is not set: this container only runs in a server built from the runtime image", softwareEnv)
	}
	var s software.Spec
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return software.Spec{}, fmt.Errorf("%s: %w", softwareEnv, err)
	}
	return s, s.Validate()
}

// javaBinary finds the runtime image's java: $JAVA_HOME/bin/java (the Temurin
// images set it), else the first java on PATH.
func javaBinary() (string, error) {
	if home := os.Getenv("JAVA_HOME"); home != "" {
		p := filepath.Join(home, "bin", "java")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return exec.LookPath("java")
}

// Install software as the game UID without capabilities or runtime secrets.
func cmdInstallServer(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("install-server", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data", defaultForwardingDataDir, "server data directory")
	cache := fs.String("cache", os.Getenv(artifactCacheEnv), "artifact cache base URL (empty downloads directly)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	spec, err := readSoftwareSpec()
	if err != nil {
		fmt.Fprintf(stderr, "felis install-server: %v\n", err)
		return 2
	}
	java, _ := javaBinary()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	in := &software.Installer{
		DataDir:  *dataDir,
		Resolver: &software.Resolver{},
		Fetch:    &artifactcache.Client{CacheURL: *cache, Log: stdout},
		Java:     java,
		Log:      stdout,
	}
	launch, err := in.Install(ctx, spec)
	if err != nil {
		fmt.Fprintf(stderr, "felis install-server: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "felis install-server: ready to start %s\n", describeLaunch(*launch))
	return 0
}

func describeLaunch(l software.Launch) string {
	switch {
	case l.Jar != "":
		return l.Jar
	case l.MainClass != "":
		return l.MainClass
	default:
		return strings.Join(l.Args, " ")
	}
}

func cmdLaunch(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("launch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data", defaultForwardingDataDir, "server data directory")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	spec, err := readSoftwareSpec()
	if err != nil {
		fmt.Fprintf(stderr, "felis launch: %v\n", err)
		return 2
	}
	rec, err := software.ReadRecord(*dataDir)
	if err != nil {
		fmt.Fprintf(stderr, "felis launch: %v\n", err)
		return 1
	}
	if rec == nil || rec.Plan.Spec.Key() != spec.Key() {
		fmt.Fprintln(stderr, "felis launch: the server software is not installed for this spec; the install step has not run")
		return 1
	}
	if err := writeServerSettings(*dataDir, os.Getenv("RCON_PORT"), os.Getenv("RCON_PASSWORD")); err != nil {
		fmt.Fprintf(stderr, "felis launch: %v\n", err)
		return 1
	}
	hibernates := os.Getenv("FELIS_HIBERNATE") == "1"
	if err := writeWatchdog(*dataDir, hibernates); err != nil {
		fmt.Fprintf(stderr, "felis launch: %v\n", err)
		return 1
	}
	flags := os.Getenv("JAVA_FLAGS")
	if hibernates {
		// Paper's own watchdog has no server.properties switch, only this property.
		flags = strings.TrimSpace(flags + " -Ddisable.watchdog=true")
	}
	java, err := javaBinary()
	if err != nil {
		fmt.Fprintf(stderr, "felis launch: no java in this image: %v\n", err)
		return 1
	}
	argv := launchArgv(java, rec.Launch, os.Getenv("JAVA_MEMORY"), flags, fs.Args())
	env := launchEnv(os.Environ(), spec)
	if err := os.Chdir(*dataDir); err != nil {
		fmt.Fprintf(stderr, "felis launch: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "felis launch: starting %s %s on Java %d\n", spec.Type, spec.MinecraftVersion, spec.Java)
	if err := syscall.Exec(java, argv, env); err != nil {
		fmt.Fprintf(stderr, "felis launch: exec java: %v\n", err)
		return 1
	}
	return 0
}

// launchArgv builds java's argv: the heap the panel sized, the owner's flags,
// the platform's JVM arguments, the launch target, then the game arguments and
// whatever the spec's args add.
func launchArgv(java string, l software.Launch, memory, flags string, extra []string) []string {
	argv := []string{java}
	if memory != "" {
		argv = append(argv, "-Xmx"+memory)
	}
	argv = append(argv, strings.Fields(flags)...)
	argv = append(argv, l.JVMArgs...)
	switch {
	case l.Jar != "":
		argv = append(argv, "-jar", l.Jar)
	case l.MainClass != "":
		if len(l.Classpath) > 0 {
			argv = append(argv, "-cp", strings.Join(l.Classpath, string(os.PathListSeparator)))
		}
		argv = append(argv, l.MainClass)
	default:
		argv = append(argv, l.Args...)
	}
	argv = append(argv, l.GameArgs...)
	return append(argv, extra...)
}

// FabricProxy-Lite reads forwarding configuration from the environment and disables incompatible chat signatures.
func launchEnv(env []string, s software.Spec) []string {
	if s.Forwarding != software.ForwardingVelocity || (s.Type != software.Fabric && s.Type != software.Quilt) {
		return env
	}
	secret := os.Getenv(forwardingSecretEnv)
	if secret == "" {
		return env
	}
	return append(env, "FABRIC_PROXY_SECRET="+secret, "FABRIC_PROXY_HACK_MESSAGE_CHAIN=true")
}

func writeServerSettings(dataDir, rconPort, rconPassword string) error {
	if err := writeFileMode(filepath.Join(dataDir, "eula.txt"), []byte("eula=true\n")); err != nil {
		return err
	}
	path := filepath.Join(dataDir, "server.properties")
	content, err := os.ReadFile(path)
	fresh := errors.Is(err, os.ErrNotExist)
	if err != nil && !fresh {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if fresh {
		// Disable vanilla's new-file whitelist by default; preserve owner changes on subsequent starts.
		content = upsertProperty(content, "white-list", "false")
	}
	content = upsertProperty(content, "server-port", strconv.Itoa(gamePort))
	content = upsertProperty(content, "online-mode", "false")
	if rconPassword != "" {
		if rconPort == "" {
			rconPort = "25575"
		}
		content = upsertProperty(content, "enable-rcon", "true")
		content = upsertProperty(content, "rcon.port", rconPort)
		content = upsertProperty(content, "rcon.password", rconPassword)
		// The platform's own RCON commands stay out of operators' chat and the log.
		content = upsertProperty(content, "broadcast-rcon-to-ops", "false")
	} else {
		content = upsertProperty(content, "enable-rcon", "false")
	}
	return writeFileMode(path, content)
}

// Copy felis into the shared emptyDir so the runtime image can remain an upstream Java image.
func cmdInstallSelf(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("install-self", flag.ContinueOnError)
	fs.SetOutput(stderr)
	to := fs.String("to", "/felis/bin", "directory to copy the felis binary into")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "felis install-self: %v\n", err)
		return 1
	}
	if err := copyExecutable(self, filepath.Join(*to, "felis")); err != nil {
		fmt.Fprintf(stderr, "felis install-self: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "felis install-self: copied to %s\n", filepath.Join(*to, "felis"))
	return 0
}

func copyExecutable(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp := dest + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o555)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

// cmdArtifactCache serves the cluster's server-software download cache (see
// internal/artifactcache) from the felis-artifacts Deployment.
func cmdArtifactCache(args []string, _, stderr io.Writer) int {
	fs := flag.NewFlagSet("artifact-cache", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", ":8090", "address to serve the cache on")
	dir := fs.String("dir", "/cache", "directory the cache keeps its files in")
	maxBytes := fs.Int64("max-bytes", 0, "evict the least recently used files beyond this many bytes (0 keeps everything)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	c := &artifactcache.Cache{Dir: *dir, MaxBytes: *maxBytes, Log: log}
	if err := c.Init(); err != nil {
		fmt.Fprintf(stderr, "felis artifact-cache: %v\n", err)
		return 1
	}
	srv := &http.Server{
		Addr:              *listen,
		Handler:           c,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Info("artifact cache listening", "addr", *listen, "dir", *dir, "maxBytes", *maxBytes)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(stderr, "felis artifact-cache: %v\n", err)
		return 1
	}
	return 0
}

// vanillaMaxTickTime is the game's default watchdog limit in milliseconds.
const vanillaMaxTickTime = "60000"

// writeWatchdog turns the game's hang watchdog off for a server that hibernates, since a long freeze looks like a hang to it, and back on when it no longer does.
func writeWatchdog(dataDir string, off bool) error {
	path := filepath.Join(dataDir, "server.properties")
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	switch {
	case off:
		content = upsertProperty(content, "max-tick-time", "-1")
	case strings.Contains("\n"+string(content), "\nmax-tick-time=-1"):
		content = upsertProperty(content, "max-tick-time", vanillaMaxTickTime)
	default:
		return nil
	}
	return writeFileMode(path, content)
}
