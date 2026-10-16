package software

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"
)

// PlanFormat versions the Plan layout. An installation recorded under another
// format is planned afresh.
const PlanFormat = 1

// Paths inside the data directory the installer manages. Everything under
// .felis is the platform's; libraries/ is the standard place both the vanilla
// bundler and the loaders put theirs.
const (
	ManagedDir    = ".felis"
	ServerDir     = ".felis/server"
	InstallerDir  = ".felis/installer"
	Log4jDir      = ".felis/log4j"
	LibrariesDir  = "libraries"
	ModsDir       = "mods"
	ManagedPrefix = "felis-managed-"
)

// Plan is what installing a Spec takes: the files to fetch, an installer to run
// for Forge and NeoForge, and how to launch the result. Planning reads upstream
// metadata; carrying the plan out needs none, so a plan recorded on the volume
// lets a server start while every metadata API is down.
type Plan struct {
	Format int  `json:"format"`
	Spec   Spec `json:"spec"`
	// Files are fetched into the data directory and checked against their
	// digests.
	Files []File `json:"files"`
	// Installer is run with `--installServer` after Files are in place.
	Installer *LoaderInstaller `json:"installer,omitempty"`
	// Paperclip marks a Paper-family jar whose first start would download the
	// vanilla jar it patches; the installer reads the jar's own record of that
	// download and fetches it ahead, through the cache.
	Paperclip bool `json:"paperclip,omitempty"`
	// Mods are the identity-forwarding mods, installed under ModsDir unless the
	// owner's own mods already carry the same mod id.
	Mods   []Mod  `json:"mods,omitempty"`
	Launch Launch `json:"launch"`
	// MojangServer is the vanilla server jar the loaders build on, for the
	// installer to place where Forge expects it.
	MojangServer *File `json:"mojangServer,omitempty"`
}

// File is one download.
type File struct {
	// Path is relative to the data directory.
	Path   string `json:"path"`
	URL    string `json:"url"`
	Digest Digest `json:"digest"`
	Size   int64  `json:"size,omitempty"`
}

// Mod is a forwarding mod: a File plus the mod id it declares.
type Mod struct {
	File
	ModID string `json:"modId"`
}

// LoaderInstaller is a Forge or NeoForge installer jar.
type LoaderInstaller struct {
	Type Type `json:"type"`
	Jar  File `json:"jar"`
	// Version is the full Maven version ("1.20.1-47.3.0", "21.4.150"), which
	// names the installer's output.
	Version string `json:"version"`
}

// Launch is how `felis launch` starts the server, after -Xmx and the owner's
// JAVA_FLAGS: JVMArgs, then either -jar Jar, or -cp Classpath MainClass, or the
// literal Args a Forge installer's run script names; then GameArgs and the
// owner's spec.args.
type Launch struct {
	JVMArgs   []string `json:"jvmArgs,omitempty"`
	Jar       string   `json:"jar,omitempty"`
	Classpath []string `json:"classpath,omitempty"`
	MainClass string   `json:"mainClass,omitempty"`
	Args      []string `json:"args,omitempty"`
	GameArgs  []string `json:"gameArgs,omitempty"`
}

// Plan resolves s into a Plan. s must already Validate.
func (r *Resolver) Plan(ctx context.Context, s Spec) (*Plan, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	p := &Plan{Format: PlanFormat, Spec: s}
	var err error
	switch s.Type {
	case Custom:
		p.Launch = Launch{Jar: s.Jar, GameArgs: []string{"nogui"}}
		return p, nil
	case Vanilla:
		err = r.planVanilla(ctx, p)
	case Paper:
		err = r.planPaper(ctx, p)
	case Purpur:
		err = r.planPurpur(ctx, p)
	case Fabric, Quilt:
		err = r.planKnot(ctx, p)
	case Forge, NeoForge:
		err = r.planForge(ctx, p)
	}
	if err != nil {
		return nil, err
	}
	log4jFiles, log4jArgs := log4jMitigation(s)
	p.Files = append(p.Files, log4jFiles...)
	p.Launch.JVMArgs = append(log4jArgs, p.Launch.JVMArgs...)
	if s.Forwarding == ForwardingVelocity {
		mods, err := r.forwardingMods(ctx, s)
		if err != nil {
			return nil, err
		}
		p.Mods = mods
	}
	return p, nil
}

// mojangServerFile resolves the vanilla server jar of version mc.
func (r *Resolver) mojangServerFile(ctx context.Context, mc string) (*File, error) {
	v, err := r.mojangVersion(ctx, mc)
	if err != nil {
		return nil, err
	}
	if v.Downloads.Server == nil {
		return nil, &NotFoundError{What: "a server download for Minecraft " + mc}
	}
	d, err := ParseDigest(SHA1, v.Downloads.Server.SHA1)
	if err != nil {
		return nil, &UpstreamError{URL: v.Downloads.Server.URL, Err: err}
	}
	if err := requireHTTPS(v.Downloads.Server.URL); err != nil {
		return nil, err
	}
	return &File{
		Path:   path.Join(ServerDir, "minecraft_server."+mc+".jar"),
		URL:    v.Downloads.Server.URL,
		Digest: d,
		Size:   v.Downloads.Server.Size,
	}, nil
}

func (r *Resolver) planVanilla(ctx context.Context, p *Plan) error {
	f, err := r.mojangServerFile(ctx, p.Spec.MinecraftVersion)
	if err != nil {
		return err
	}
	p.Files = append(p.Files, *f)
	p.Launch = Launch{Jar: f.Path, GameArgs: []string{"nogui"}}
	return nil
}

func (r *Resolver) planPaper(ctx context.Context, p *Plan) error {
	var b struct {
		Downloads map[string]struct {
			Name      string `json:"name"`
			Checksums struct {
				SHA256 string `json:"sha256"`
			} `json:"checksums"`
			Size int64  `json:"size"`
			URL  string `json:"url"`
		} `json:"downloads"`
	}
	u := fmt.Sprintf("%s/v3/projects/paper/versions/%s/builds/%s", r.url(r.Fill, FillURL),
		url.PathEscape(p.Spec.MinecraftVersion), url.PathEscape(p.Spec.Build))
	if err := r.getJSON(ctx, u, &b); err != nil {
		return err
	}
	dl, ok := b.Downloads["server:default"]
	if !ok {
		return &UpstreamError{URL: u, Err: fmt.Errorf("no server:default download")}
	}
	d, err := ParseDigest(SHA256, dl.Checksums.SHA256)
	if err != nil {
		return &UpstreamError{URL: u, Err: err}
	}
	if err := requireHTTPS(dl.URL); err != nil {
		return err
	}
	f := File{
		Path:   path.Join(ServerDir, fmt.Sprintf("paper-%s-%s.jar", p.Spec.MinecraftVersion, p.Spec.Build)),
		URL:    dl.URL,
		Digest: d,
		Size:   dl.Size,
	}
	p.Files = append(p.Files, f)
	p.Paperclip = true
	p.Launch = Launch{Jar: f.Path, GameArgs: []string{"nogui"}}
	return nil
}

func (r *Resolver) planPurpur(ctx context.Context, p *Plan) error {
	base := fmt.Sprintf("%s/v2/purpur/%s/%s", r.url(r.Purpur, PurpurURL),
		url.PathEscape(p.Spec.MinecraftVersion), url.PathEscape(p.Spec.Build))
	var b struct {
		Result string `json:"result"`
		MD5    string `json:"md5"`
	}
	if err := r.getJSON(ctx, base, &b); err != nil {
		return err
	}
	if b.Result != "" && b.Result != "SUCCESS" {
		return &NotFoundError{What: fmt.Sprintf("a successful Purpur build %s for %s", p.Spec.Build, p.Spec.MinecraftVersion)}
	}
	d, err := ParseDigest(MD5, b.MD5)
	if err != nil {
		return &UpstreamError{URL: base, Err: err}
	}
	f := File{
		Path:   path.Join(ServerDir, fmt.Sprintf("purpur-%s-%s.jar", p.Spec.MinecraftVersion, p.Spec.Build)),
		URL:    base + "/download",
		Digest: d,
	}
	p.Files = append(p.Files, f)
	p.Paperclip = true
	p.Launch = Launch{Jar: f.Path, GameArgs: []string{"nogui"}}
	return nil
}

// knotProfile is the launcher profile Fabric and Quilt meta serve for a loader
// and game version.
type knotProfile struct {
	MainClass string `json:"mainClass"`
	Libraries []struct {
		Name   string `json:"name"`
		URL    string `json:"url"`
		SHA1   string `json:"sha1"`
		SHA256 string `json:"sha256"`
		Size   int64  `json:"size"`
	} `json:"libraries"`
}

// planKnot plans a Fabric or Quilt server the way their installers lay one out:
// every library of the loader's server profile under libraries/, the vanilla jar
// beside them, and Knot started directly with the game jar named by system
// property. The profile's libraries carry a sha256 where meta knows one; the
// rest (the loader and intermediary jars) are checked against the digest file
// their Maven repository serves.
func (r *Resolver) planKnot(ctx context.Context, p *Plan) error {
	s := p.Spec
	var u, gameJarProperty string
	if s.Type == Fabric {
		u = fmt.Sprintf("%s/v2/versions/loader/%s/%s/server/json", r.url(r.FabricMeta, FabricMetaURL), url.PathEscape(s.MinecraftVersion), url.PathEscape(s.Build))
		gameJarProperty = "fabric.gameJarPath"
	} else {
		u = fmt.Sprintf("%s/v3/versions/loader/%s/%s/server/json", r.url(r.QuiltMeta, QuiltMetaURL), url.PathEscape(s.MinecraftVersion), url.PathEscape(s.Build))
		gameJarProperty = "loader.gameJarPath"
	}
	var prof knotProfile
	if err := r.getJSON(ctx, u, &prof); err != nil {
		return err
	}
	if prof.MainClass == "" || len(prof.Libraries) == 0 {
		return &UpstreamError{URL: u, Err: fmt.Errorf("profile has no main class or libraries")}
	}
	for _, lib := range prof.Libraries {
		coord, err := ParseMaven(lib.Name)
		if err != nil {
			return &UpstreamError{URL: u, Err: err}
		}
		if err := requireHTTPS(lib.URL); err != nil {
			return err
		}
		artifact := strings.TrimRight(lib.URL, "/") + "/" + coord.Path()
		var d Digest
		switch {
		case lib.SHA256 != "":
			d, err = ParseDigest(SHA256, lib.SHA256)
		case lib.SHA1 != "":
			d, err = ParseDigest(SHA1, lib.SHA1)
		default:
			d, err = r.sidecarDigest(ctx, artifact)
		}
		if err != nil {
			return err
		}
		f := File{Path: path.Join(LibrariesDir, coord.Path()), URL: artifact, Digest: d, Size: lib.Size}
		p.Files = append(p.Files, f)
		p.Launch.Classpath = append(p.Launch.Classpath, f.Path)
	}
	game, err := r.mojangServerFile(ctx, s.MinecraftVersion)
	if err != nil {
		return err
	}
	p.Files = append(p.Files, *game)
	p.Launch.JVMArgs = []string{"-D" + gameJarProperty + "=" + game.Path}
	p.Launch.MainClass = prof.MainClass
	p.Launch.GameArgs = []string{"nogui"}
	return nil
}

// planForge plans a Forge or NeoForge server: the installer, checked against
// its Maven digest, and the vanilla jar it patches. The libraries the installer
// needs are read out of the installer jar at install time (ForgeLibraries) and
// fetched ahead, through the cache, so the installer finds them present.
func (r *Resolver) planForge(ctx context.Context, p *Plan) error {
	s := p.Spec
	var full, artifact string
	if s.Type == Forge {
		full = s.MinecraftVersion + "-" + s.Build
		artifact = fmt.Sprintf("%s/net/minecraftforge/forge/%s/forge-%s-installer.jar", r.url(r.ForgeMaven, ForgeMavenURL), full, full)
	} else {
		full = s.Build
		artifact = fmt.Sprintf("%s/releases/net/neoforged/neoforge/%s/neoforge-%s-installer.jar", r.url(r.NeoForgeMaven, NeoForgeMavenURL), full, full)
	}
	d, err := r.sidecarDigest(ctx, artifact)
	if err != nil {
		return err
	}
	p.Installer = &LoaderInstaller{
		Type:    s.Type,
		Version: full,
		Jar:     File{Path: path.Join(InstallerDir, path.Base(artifact)), URL: artifact, Digest: d},
	}
	game, err := r.mojangServerFile(ctx, s.MinecraftVersion)
	if err != nil {
		return err
	}
	p.MojangServer = game
	p.Launch.GameArgs = []string{"nogui"}
	return nil
}

// requireHTTPS refuses a download URL upstream metadata handed out over plain
// HTTP: the digest would still catch a swapped file, but nothing the installer
// fetches needs to travel in the clear.
func requireHTTPS(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return &UpstreamError{URL: raw, Err: fmt.Errorf("download URL is not https")}
	}
	return nil
}

// Maven is a Maven coordinate.
type Maven struct {
	Group, Artifact, Version, Classifier, Ext string
}

// ParseMaven parses "group:artifact:version[:classifier][@ext]".
func ParseMaven(name string) (Maven, error) {
	main, ext, _ := strings.Cut(name, "@")
	if ext == "" {
		ext = "jar"
	}
	parts := strings.Split(main, ":")
	if len(parts) < 3 || len(parts) > 4 {
		return Maven{}, fmt.Errorf("software: bad maven coordinate %q", name)
	}
	m := Maven{Group: parts[0], Artifact: parts[1], Version: parts[2], Ext: ext}
	if len(parts) == 4 {
		m.Classifier = parts[3]
	}
	for _, f := range []string{m.Group, m.Artifact, m.Version, m.Classifier, m.Ext} {
		if strings.Contains(f, "/") || strings.Contains(f, "..") || strings.Contains(f, "\\") {
			return Maven{}, fmt.Errorf("software: bad maven coordinate %q", name)
		}
	}
	return m, nil
}

// Path is the coordinate's repository path.
func (m Maven) Path() string {
	file := m.Artifact + "-" + m.Version
	if m.Classifier != "" {
		file += "-" + m.Classifier
	}
	file += "." + m.Ext
	return strings.ReplaceAll(m.Group, ".", "/") + "/" + m.Artifact + "/" + m.Version + "/" + file
}

// Log4Shell (CVE-2021-44228) mitigation, from Mojang's advisory of 2021-12-10:
// 1.18 and 1.17 take -Dlog4j2.formatMsgNoLookups=true; 1.12 to 1.16.5 and 1.7 to
// 1.11.2 run with the logging configuration Mojang published for each range.
// Paper, Purpur and Forge shipped fixed builds of every version they still
// served; the property is added for them too, since it costs nothing.
var (
	log4jConfig112 = File{
		Path:   path.Join(Log4jDir, "log4j2_112-116.xml"),
		URL:    "https://launcher.mojang.com/v1/objects/02937d122c86ce73319ef9975b58896fc1b491d1/log4j2_112-116.xml",
		Digest: Digest{Algo: SHA1, Hex: "02937d122c86ce73319ef9975b58896fc1b491d1"},
	}
	log4jConfig17 = File{
		Path:   path.Join(Log4jDir, "log4j2_17-111.xml"),
		URL:    "https://launcher.mojang.com/v1/objects/4bb89a97a66f350bc9f73b3ca8509632682aea2e/log4j2_17-111.xml",
		Digest: Digest{Algo: SHA1, Hex: "4bb89a97a66f350bc9f73b3ca8509632682aea2e"},
	}
)

func log4jMitigation(s Spec) ([]File, []string) {
	mc := s.MinecraftVersion
	if _, ok := parseMC(mc); !ok || AtLeast(mc, "1.18.1") || !AtLeast(mc, "1.7") {
		return nil, nil
	}
	lookups := "-Dlog4j2.formatMsgNoLookups=true"
	vanillaLogging := s.Type == Vanilla || s.Type == Fabric || s.Type == Quilt
	switch {
	case !vanillaLogging || AtLeast(mc, "1.17"):
		return nil, []string{lookups}
	case AtLeast(mc, "1.12"):
		return []File{log4jConfig112}, []string{lookups, "-Dlog4j.configurationFile=" + log4jConfig112.Path}
	default:
		return []File{log4jConfig17}, []string{lookups, "-Dlog4j.configurationFile=" + log4jConfig17.Path}
	}
}

// Modrinth projects of the forwarding mods. IDs, not slugs: a slug can be
// renamed or taken over, a project id cannot.
const (
	ModrinthFabricProxyLite  = "8dI2tmqs" // FabricProxy-Lite, fabric.mod.json id "fabricproxy-lite"
	ModrinthFabricAPI        = "P7dR8mSH" // Fabric API, fabric.mod.json id "fabric-api"
	ModrinthQuiltedFabricAPI = "qvIfYCYJ" // Quilted Fabric API, quilt.mod.json id "quilted_fabric_api"
	ModrinthPCF              = "vDyrHl8l" // Proxy Compatible Forge, mods.toml modId "pcf"
)

// forwardingMod names one mod a type needs for Velocity forwarding.
type forwardingMod struct {
	project string
	loader  string
	modID   string
}

func forwardingModsFor(t Type) []forwardingMod {
	switch t {
	case Fabric:
		return []forwardingMod{{ModrinthFabricProxyLite, "fabric", "fabricproxy-lite"}, {ModrinthFabricAPI, "fabric", "fabric-api"}}
	case Quilt:
		// Quilt loads Fabric mods, and FabricProxy-Lite's recent releases list
		// only the fabric loader on Modrinth. Fabric API itself asks for a newer
		// fabricloader than Quilt's loader stands in for, so Quilt takes Quilted
		// Fabric API, which carries the same modules.
		return []forwardingMod{{ModrinthFabricProxyLite, "fabric", "fabricproxy-lite"}, {ModrinthQuiltedFabricAPI, "quilt", "quilted_fabric_api"}}
	case Forge:
		return []forwardingMod{{ModrinthPCF, "forge", "pcf"}}
	case NeoForge:
		return []forwardingMod{{ModrinthPCF, "neoforge", "pcf"}}
	}
	return nil
}

// ForwardingFor decides whether a server can take the proxy's player identity.
// The Paper family reads Velocity forwarding from its own config from 1.13 on.
// The modded loaders need a forwarding mod published for their exact version:
// FabricProxy-Lite (with Fabric API) for Fabric and Quilt, Proxy Compatible
// Forge for Forge from 1.15.2 and NeoForge. Modrinth lists PCF for older Forge
// too, but there it needs a mixin loader mod (MixinBootstrap, ModernMixins or
// MixinBooter, per its README), so those versions are left out. A vanilla
// server and a custom jar have no way to take it.
func (r *Resolver) ForwardingFor(ctx context.Context, s Spec) (string, error) {
	switch s.Type {
	case Paper, Purpur:
		if AtLeast(s.MinecraftVersion, "1.13") {
			return ForwardingVelocity, nil
		}
		return ForwardingNone, nil
	case Forge:
		if !AtLeast(s.MinecraftVersion, "1.15.2") {
			return ForwardingNone, nil
		}
	case Fabric, Quilt, NeoForge:
	default:
		return ForwardingNone, nil
	}
	for _, m := range forwardingModsFor(s.Type) {
		f, err := r.modrinthLatest(ctx, m.project, m.loader, s.MinecraftVersion)
		if err != nil {
			return "", err
		}
		if f == nil {
			return ForwardingNone, nil
		}
	}
	return ForwardingVelocity, nil
}

func (r *Resolver) forwardingMods(ctx context.Context, s Spec) ([]Mod, error) {
	var out []Mod
	for _, m := range forwardingModsFor(s.Type) {
		f, err := r.modrinthLatest(ctx, m.project, m.loader, s.MinecraftVersion)
		if err != nil {
			return nil, err
		}
		if f == nil {
			return nil, &NotFoundError{What: fmt.Sprintf("a forwarding mod (Modrinth %s) for %s %s", m.project, s.Type, s.MinecraftVersion)}
		}
		f.Path = path.Join(ModsDir, ManagedPrefix+path.Base(f.Path))
		out = append(out, Mod{File: *f, ModID: m.modID})
	}
	return out, nil
}

// modrinthLatest returns the newest release file of a Modrinth project for a
// loader and game version, or nil when the project publishes none.
func (r *Resolver) modrinthLatest(ctx context.Context, project, loader, mc string) (*File, error) {
	q := url.Values{}
	q.Set("loaders", `["`+loader+`"]`)
	q.Set("game_versions", `["`+mc+`"]`)
	u := fmt.Sprintf("%s/v2/project/%s/version?%s", r.url(r.Modrinth, ModrinthURL), project, q.Encode())
	var versions []struct {
		VersionType   string    `json:"version_type"`
		DatePublished time.Time `json:"date_published"`
		Files         []struct {
			URL      string            `json:"url"`
			Filename string            `json:"filename"`
			Primary  bool              `json:"primary"`
			Size     int64             `json:"size"`
			Hashes   map[string]string `json:"hashes"`
		} `json:"files"`
	}
	if err := r.getJSON(ctx, u, &versions); err != nil {
		var nf *NotFoundError
		if asNotFound(err, &nf) {
			return nil, nil
		}
		return nil, err
	}
	sort.SliceStable(versions, func(i, j int) bool {
		ri, rj := versions[i].VersionType == "release", versions[j].VersionType == "release"
		if ri != rj {
			return ri
		}
		return versions[i].DatePublished.After(versions[j].DatePublished)
	})
	for _, v := range versions {
		for _, f := range v.Files {
			if !f.Primary && len(v.Files) > 1 {
				continue
			}
			d, err := ParseDigest(SHA512, f.Hashes["sha512"])
			if err != nil {
				return nil, &UpstreamError{URL: u, Err: err}
			}
			if err := requireHTTPS(f.URL); err != nil {
				return nil, err
			}
			name := path.Base(f.Filename)
			if name == "." || name == "/" || !strings.HasSuffix(name, ".jar") {
				return nil, &UpstreamError{URL: u, Err: fmt.Errorf("unexpected file name %q", f.Filename)}
			}
			return &File{Path: name, URL: f.URL, Digest: d, Size: f.Size}, nil
		}
	}
	return nil, nil
}
