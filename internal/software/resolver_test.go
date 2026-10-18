package software

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

var ctx = context.Background()

func TestPlanPaperTakesFillsDownload(t *testing.T) {
	up := newFakeUpstream(t)
	var fill struct {
		Downloads map[string]struct {
			URL       string `json:"url"`
			Size      int64  `json:"size"`
			Checksums struct {
				SHA256 string `json:"sha256"`
			} `json:"checksums"`
		} `json:"downloads"`
	}
	if err := json.Unmarshal(up.read("fill/1.21.4-232.json"), &fill); err != nil {
		t.Fatal(err)
	}
	want := fill.Downloads["server:default"]

	p, err := up.resolver().Plan(ctx, Spec{Type: Paper, MinecraftVersion: "1.21.4", Build: "232", Java: 21})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Files) != 1 {
		t.Fatalf("files = %+v, want the one server jar", p.Files)
	}
	f := p.Files[0]
	if f.URL != want.URL || f.Digest != (Digest{SHA256, want.Checksums.SHA256}) || f.Size != want.Size {
		t.Errorf("paper jar = %+v, want Fill's %s sha256:%s (%d bytes)", f, want.URL, want.Checksums.SHA256, want.Size)
	}
	if !p.Paperclip || p.Launch.Jar != f.Path {
		t.Errorf("launch = %+v (paperclip %v), want -jar %s with the prefetch", p.Launch, p.Paperclip, f.Path)
	}
}

func TestPlanPurpurTakesItsMD5(t *testing.T) {
	up := newFakeUpstream(t)
	var b struct{ MD5 string }
	_ = json.Unmarshal(up.read("purpur/1.21.4-2416.json"), &b)
	p, err := up.resolver().Plan(ctx, Spec{Type: Purpur, MinecraftVersion: "1.21.4", Build: "2416", Java: 21})
	if err != nil {
		t.Fatal(err)
	}
	f := p.Files[0]
	if f.URL != "https://api.purpurmc.org/v2/purpur/1.21.4/2416/download" || f.Digest != (Digest{MD5, b.MD5}) {
		t.Errorf("purpur jar = %+v, want the build's download by md5 %s", f, b.MD5)
	}
}

func TestPlanVanillaChecksTheVersionJSONAgainstTheManifest(t *testing.T) {
	up := newFakeUpstream(t)
	var v struct {
		Downloads struct {
			Server struct {
				SHA1 string `json:"sha1"`
				URL  string `json:"url"`
				Size int64  `json:"size"`
			} `json:"server"`
		} `json:"downloads"`
	}
	_ = json.Unmarshal(up.read("mojang/1.21.4.json"), &v)
	p, err := up.resolver().Plan(ctx, Spec{Type: Vanilla, MinecraftVersion: "1.21.4", Java: 21})
	if err != nil {
		t.Fatal(err)
	}
	if f := p.Files[0]; f.URL != v.Downloads.Server.URL || f.Digest != (Digest{SHA1, v.Downloads.Server.SHA1}) {
		t.Errorf("vanilla jar = %+v, want %s sha1:%s", f, v.Downloads.Server.URL, v.Downloads.Server.SHA1)
	}

	tampered := up.read("mojang/1.21.4.json")
	tampered = []byte(strings.Replace(string(tampered), v.Downloads.Server.SHA1, strings.Repeat("0", 40), 1))
	for key, name := range up.routes {
		if name == "mojang/1.21.4.json" {
			up.override[key] = tampered
		}
	}
	if _, err := up.resolver().Plan(ctx, Spec{Type: Vanilla, MinecraftVersion: "1.21.4", Java: 21}); !errors.Is(err, ErrDigestMismatch) {
		t.Errorf("plan from a tampered version JSON: err = %v, want ErrDigestMismatch", err)
	}
}

func TestRequiredJava(t *testing.T) {
	r := newFakeUpstream(t).resolver()
	for _, tc := range []struct {
		t    Type
		mc   string
		want int
	}{
		{Vanilla, "1.12.2", 8}, {Vanilla, "1.16.5", 8}, {Vanilla, "1.20.1", 17}, {Vanilla, "1.21.4", 21}, {Vanilla, "26.3", 25},
		{Paper, "1.21.4", 21}, {Paper, "1.12.2", 8},
	} {
		got, err := r.RequiredJava(ctx, tc.t, tc.mc)
		if err != nil || got != tc.want {
			t.Errorf("RequiredJava(%s %s) = %d, %v; want %d", tc.t, tc.mc, got, err, tc.want)
		}
	}
}

func TestRuntimeFor(t *testing.T) {
	for req, want := range map[int]int{0: 8, 8: 8, 16: 17, 17: 17, 21: 21, 25: 25} {
		if got, err := RuntimeFor(req); err != nil || got != want {
			t.Errorf("RuntimeFor(%d) = %d, %v; want %d", req, got, err, want)
		}
	}
	if _, err := RuntimeFor(26); err == nil {
		t.Error("RuntimeFor(26) found a runtime; the platform ships none that new")
	}
}

func TestPlanFabricFollowsTheServerProfile(t *testing.T) {
	up := newFakeUpstream(t)
	var prof struct {
		MainClass string `json:"mainClass"`
		Libraries []struct {
			Name   string `json:"name"`
			SHA256 string `json:"sha256"`
		} `json:"libraries"`
	}
	_ = json.Unmarshal(up.read("fabric/server-1.21.4-0.19.5.json"), &prof)
	p, err := up.resolver().Plan(ctx, Spec{Type: Fabric, MinecraftVersion: "1.21.4", Build: "0.19.5", Java: 21})
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]File{}
	for _, f := range p.Files {
		byPath[f.Path] = f
	}
	if asm := byPath["libraries/org/ow2/asm/asm/9.10.1/asm-9.10.1.jar"]; asm.Digest != (Digest{SHA256, prof.Libraries[0].SHA256}) ||
		asm.URL != "https://maven.fabricmc.net/org/ow2/asm/asm/9.10.1/asm-9.10.1.jar" {
		t.Errorf("asm = %+v, want profile sha256 %s from maven.fabricmc.net", asm, prof.Libraries[0].SHA256)
	}
	if l := byPath["libraries/net/fabricmc/fabric-loader/0.19.5/fabric-loader-0.19.5.jar"]; l.Digest != (Digest{SHA256, up.sidecar("fabric-loader-0.19.5.jar.sha256")}) {
		t.Errorf("fabric-loader = %+v, want the sha256 its Maven serves", l)
	}
	if l := byPath["libraries/net/fabricmc/intermediary/1.21.4/intermediary-1.21.4.jar"]; l.Digest != (Digest{SHA256, up.sidecar("intermediary-1.21.4.jar.sha256")}) {
		t.Errorf("intermediary = %+v, want the sha256 its Maven serves", l)
	}
	if len(p.Launch.Classpath) != len(prof.Libraries) || p.Launch.MainClass != prof.MainClass {
		t.Errorf("launch = %+v, want the %d profile libraries and main class %s", p.Launch, len(prof.Libraries), prof.MainClass)
	}
	game := byPath[".felis/server/minecraft_server.1.21.4.jar"]
	if game.URL == "" || !slices.Contains(p.Launch.JVMArgs, "-Dfabric.gameJarPath="+game.Path) {
		t.Errorf("game jar %+v, jvm args %v: want Knot pointed at the vanilla jar", game, p.Launch.JVMArgs)
	}
}

func TestPlanForgeNamesTheInstallerAndTheVanillaJar(t *testing.T) {
	up := newFakeUpstream(t)
	p, err := up.resolver().Plan(ctx, Spec{Type: Forge, MinecraftVersion: "1.20.1", Build: "47.4.10", Java: 17})
	if err != nil {
		t.Fatal(err)
	}
	inst := p.Installer
	if inst == nil || inst.Version != "1.20.1-47.4.10" ||
		inst.Jar.URL != "https://maven.minecraftforge.net/net/minecraftforge/forge/1.20.1-47.4.10/forge-1.20.1-47.4.10-installer.jar" ||
		inst.Jar.Digest != (Digest{SHA256, up.sidecar("forge-1.20.1-47.4.10-installer.jar.sha256")}) {
		t.Errorf("installer = %+v", inst)
	}
	if p.MojangServer == nil || p.MojangServer.Digest.Hex != "84194a2f286ef7c14ed7ce0090dba59902951553" {
		t.Errorf("vanilla jar = %+v, want Mojang's 1.20.1 server by its sha1", p.MojangServer)
	}

	p, err = up.resolver().Plan(ctx, Spec{Type: NeoForge, MinecraftVersion: "1.21.4", Build: "21.4.150", Java: 21})
	if err != nil {
		t.Fatal(err)
	}
	if p.Installer.Jar.URL != "https://maven.neoforged.net/releases/net/neoforged/neoforge/21.4.150/neoforge-21.4.150-installer.jar" ||
		p.Installer.Jar.Digest.Hex != up.sidecar("neoforge-21.4.150-installer.jar.sha256") {
		t.Errorf("neoforge installer = %+v", p.Installer.Jar)
	}
}

func TestQuiltBuildsNewestFirst(t *testing.T) {
	r := newFakeUpstream(t).resolver()
	b, err := r.RecommendedBuild(ctx, Quilt, "1.20.1")
	if err != nil || b != "0.30.1" {
		t.Errorf("recommended quilt loader = %q, %v; want 0.30.1", b, err)
	}
	builds, _ := r.Builds(ctx, Quilt, "1.20.1")
	if builds[0].ID != "0.31.0-beta.4" || builds[0].Stable {
		t.Errorf("first build = %+v; the list must run newest first, betas included (0.31.0-beta.4 when captured)", builds[0])
	}
}

func TestForgeBuildsMarkForgesRecommendation(t *testing.T) {
	builds, err := newFakeUpstream(t).resolver().Builds(ctx, Forge, "1.20.1")
	if err != nil {
		t.Fatal(err)
	}
	if builds[0].ID != "47.4.28" {
		t.Errorf("first build = %s, want the newest, 47.4.28", builds[0].ID)
	}
	for _, b := range builds {
		if b.Recommended != (b.ID == "47.4.10") {
			t.Errorf("build %s recommended = %v; Forge recommends 47.4.10 for 1.20.1", b.ID, b.Recommended)
		}
	}
}

func TestNeoForgeVersionsMapToMinecraft(t *testing.T) {
	r := newFakeUpstream(t).resolver()
	vs, err := r.Versions(ctx, NeoForge)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, v := range vs {
		ids[v.ID] = true
	}
	for _, want := range []string{"26.3", "1.21.4", "1.21", "1.20.2"} {
		if !ids[want] {
			t.Errorf("NeoForge versions miss %s: %v", want, vs)
		}
	}
	if ids["1.20.1"] {
		t.Error("NeoForge 1.20.1 shipped as net.neoforged:forge, which this resolver does not install")
	}
	builds, err := r.Builds(ctx, NeoForge, "26.3")
	if err != nil {
		t.Fatal(err)
	}
	if builds[0].ID != "26.3.0.85" || !builds[0].Recommended {
		t.Errorf("26.3 builds start %+v, want 26.3.0.85 recommended", builds[0])
	}
	if rec, _ := r.RecommendedBuild(ctx, NeoForge, "26.3"); rec != "26.3.0.85" {
		t.Errorf("recommended = %s", rec)
	}
}

func TestForwardingFor(t *testing.T) {
	r := newFakeUpstream(t).resolver()
	for _, tc := range []struct {
		s    Spec
		want string
	}{
		{Spec{Type: Paper, MinecraftVersion: "1.12.2"}, ForwardingNone},
		{Spec{Type: Paper, MinecraftVersion: "1.13"}, ForwardingVelocity},
		{Spec{Type: Purpur, MinecraftVersion: "1.21.4"}, ForwardingVelocity},
		{Spec{Type: Fabric, MinecraftVersion: "1.21.4"}, ForwardingVelocity},
		{Spec{Type: Quilt, MinecraftVersion: "1.20.1"}, ForwardingVelocity},
		{Spec{Type: Quilt, MinecraftVersion: "1.21.4"}, ForwardingNone},
		{Spec{Type: Forge, MinecraftVersion: "1.20.1"}, ForwardingVelocity},
		{Spec{Type: Forge, MinecraftVersion: "26.3"}, ForwardingNone},
		{Spec{Type: Forge, MinecraftVersion: "1.12.2"}, ForwardingNone},
		// Modrinth lists PCF 1.3.1 for both; its README asks for MixinBootstrap
		// on Forge 1.14 to 1.15.1 and nothing extra from 1.15.2.
		{Spec{Type: Forge, MinecraftVersion: "1.14.4"}, ForwardingNone},
		{Spec{Type: Forge, MinecraftVersion: "1.15.2"}, ForwardingVelocity},
		{Spec{Type: NeoForge, MinecraftVersion: "1.21.4"}, ForwardingVelocity},
		{Spec{Type: Vanilla, MinecraftVersion: "1.21.4"}, ForwardingNone},
		{Spec{Type: Custom}, ForwardingNone},
	} {
		got, err := r.ForwardingFor(ctx, tc.s)
		if err != nil || got != tc.want {
			t.Errorf("ForwardingFor(%s %s) = %q, %v; want %q", tc.s.Type, tc.s.MinecraftVersion, got, err, tc.want)
		}
	}
}

func TestPlanAddsTheForwardingMods(t *testing.T) {
	up := newFakeUpstream(t)
	p, err := up.resolver().Plan(ctx, Spec{Type: Fabric, MinecraftVersion: "1.21.4", Build: "0.19.5", Java: 21, Forwarding: ForwardingVelocity})
	if err != nil {
		t.Fatal(err)
	}
	var fpl []struct {
		Files []struct {
			URL      string            `json:"url"`
			Filename string            `json:"filename"`
			Hashes   map[string]string `json:"hashes"`
		} `json:"files"`
	}
	_ = json.Unmarshal(up.read("modrinth/8dI2tmqs-fabric-1.21.4.json"), &fpl)
	want := fpl[0].Files[0]
	if len(p.Mods) != 2 {
		t.Fatalf("mods = %+v, want FabricProxy-Lite and Fabric API", p.Mods)
	}
	m := p.Mods[0]
	if m.ModID != "fabricproxy-lite" || m.URL != want.URL || m.Digest != (Digest{SHA512, want.Hashes["sha512"]}) ||
		m.Path != "mods/"+ManagedPrefix+want.Filename {
		t.Errorf("FabricProxy-Lite = %+v, want %s by sha512 at mods/%s%s", m, want.URL, ManagedPrefix, want.Filename)
	}
	if p.Mods[1].ModID != "fabric-api" {
		t.Errorf("second mod = %+v, want Fabric API", p.Mods[1])
	}
}

func TestLog4jMitigationFollowsMojangsAdvisory(t *testing.T) {
	for _, tc := range []struct {
		s       Spec
		config  string
		lookups bool
	}{
		{Spec{Type: Vanilla, MinecraftVersion: "1.16.5"}, "log4j2_112-116.xml", true},
		{Spec{Type: Fabric, MinecraftVersion: "1.12.2"}, "log4j2_112-116.xml", true},
		{Spec{Type: Vanilla, MinecraftVersion: "1.8.9"}, "log4j2_17-111.xml", true},
		{Spec{Type: Vanilla, MinecraftVersion: "1.17.1"}, "", true},
		{Spec{Type: Vanilla, MinecraftVersion: "1.18"}, "", true},
		{Spec{Type: Vanilla, MinecraftVersion: "1.18.1"}, "", false},
		{Spec{Type: Vanilla, MinecraftVersion: "26.3"}, "", false},
		{Spec{Type: Paper, MinecraftVersion: "1.16.5"}, "", true},
	} {
		files, args := log4jMitigation(tc.s)
		gotLookups := slices.Contains(args, "-Dlog4j2.formatMsgNoLookups=true")
		var gotConfig string
		if len(files) == 1 {
			gotConfig = strings.TrimPrefix(files[0].Path, Log4jDir+"/")
			if !slices.Contains(args, "-Dlog4j.configurationFile="+files[0].Path) {
				t.Errorf("%s %s: config %s fetched but not used: %v", tc.s.Type, tc.s.MinecraftVersion, gotConfig, args)
			}
		}
		if gotConfig != tc.config || gotLookups != tc.lookups {
			t.Errorf("%s %s: config %q lookups %v, want %q %v", tc.s.Type, tc.s.MinecraftVersion, gotConfig, gotLookups, tc.config, tc.lookups)
		}
	}
}

func TestValidateJarPath(t *testing.T) {
	for _, ok := range []string{"server.jar", "mods-pack/start.jar", "Server.JAR"} {
		if err := ValidateJarPath(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "/data/server.jar", "../server.jar", "a/../../b.jar", "./server.jar", "server.zip",
		".felis/installed.jar", "a\\b.jar", "a//b.jar", "srv\x00.jar"} {
		if err := ValidateJarPath(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestSpecValidate(t *testing.T) {
	for _, s := range []Spec{
		{Type: "bukkit", MinecraftVersion: "1.21.4", Java: 21},
		{Type: Paper, MinecraftVersion: "1.21.4", Build: "232", Java: 11},
		{Type: Paper, MinecraftVersion: "1.21.4", Java: 21},
		{Type: Vanilla, MinecraftVersion: "1.21.4", Build: "1", Java: 21},
		{Type: Paper, MinecraftVersion: "1.21.4/../x", Build: "232", Java: 21},
		{Type: Custom, Jar: "server.jar", MinecraftVersion: "1.21.4", Java: 21},
		{Type: Custom, Jar: "server.jar", Java: 21, Forwarding: ForwardingVelocity},
		{Type: Paper, MinecraftVersion: "1.21.4", Build: "232", Java: 21, Jar: "x.jar"},
	} {
		if err := s.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: err = %v, want ErrInvalid", s, err)
		}
	}
}

func TestUpstreamOutageIsNotANotFound(t *testing.T) {
	up := newFakeUpstream(t)
	up.down = true
	_, err := up.resolver().Versions(ctx, Paper)
	var nf *NotFoundError
	var ue *UpstreamError
	if errors.As(err, &nf) || !errors.As(err, &ue) {
		t.Errorf("err = %v, want an UpstreamError", err)
	}
}
