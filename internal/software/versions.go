package software

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

type Version struct {
	ID     string `json:"id"`
	Stable bool   `json:"stable"`
}

type Build struct {
	ID          string `json:"id"`
	Stable      bool   `json:"stable"`
	Recommended bool   `json:"recommended,omitempty"`
}

// minVersion limits each type to installer paths supported and exercised by this build.
var minVersion = map[Type]string{
	Vanilla:  "1.7.10",
	Paper:    "1.8.8",
	Purpur:   "1.14.1",
	Fabric:   "1.14",
	Quilt:    "1.14.4",
	Forge:    "1.12.2",
	NeoForge: "1.20.2",
}

// offered applies release minimums but accepts non-release snapshot IDs supplied by the upstream type.
func offered(t Type, v string) bool {
	min, ok := minVersion[t]
	if !ok {
		return false
	}
	if _, ok := parseMC(v); !ok {
		return t == Vanilla || t == Fabric || t == Quilt
	}
	return AtLeast(v, min)
}

func (r *Resolver) Versions(ctx context.Context, t Type) ([]Version, error) {
	var out []Version
	switch t {
	case Vanilla:
		m, err := r.mojangManifest(ctx)
		if err != nil {
			return nil, err
		}
		for _, v := range m.Versions {
			if v.Type != "release" && v.Type != "snapshot" {
				continue
			}
			out = append(out, Version{ID: v.ID, Stable: v.Type == "release"})
		}
		// Preserve manifest order because snapshot IDs cannot be sorted as release numbers.
		return filterOffered(t, out), nil
	case Paper:
		var p struct {
			Versions map[string][]string `json:"versions"`
		}
		if err := r.getJSON(ctx, r.url(r.Fill, FillURL)+"/v3/projects/paper", &p); err != nil {
			return nil, err
		}
		for _, group := range p.Versions {
			for _, v := range group {
				out = append(out, Version{ID: v, Stable: stableVersion(v)})
			}
		}
	case Purpur:
		var p struct {
			Versions []string `json:"versions"`
		}
		if err := r.getJSON(ctx, r.url(r.Purpur, PurpurURL)+"/v2/purpur", &p); err != nil {
			return nil, err
		}
		for _, v := range p.Versions {
			out = append(out, Version{ID: v, Stable: stableVersion(v)})
		}
	case Fabric, Quilt:
		var games []struct {
			Version string `json:"version"`
			Stable  bool   `json:"stable"`
		}
		base, path := r.url(r.FabricMeta, FabricMetaURL), "/v2/versions/game"
		if t == Quilt {
			base, path = r.url(r.QuiltMeta, QuiltMetaURL), "/v3/versions/game"
		}
		if err := r.getJSON(ctx, base+path, &games); err != nil {
			return nil, err
		}
		for _, g := range games {
			out = append(out, Version{ID: g.Version, Stable: g.Stable})
		}
		return filterOffered(t, out), nil
	case Forge:
		index, err := r.forgeIndex(ctx)
		if err != nil {
			return nil, err
		}
		for mc := range index {
			out = append(out, Version{ID: mc, Stable: stableVersion(mc)})
		}
	case NeoForge:
		builds, err := r.neoForgeVersions(ctx)
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		stable := map[string]bool{}
		for _, b := range builds {
			mc, ok := neoForgeMinecraft(b)
			if !ok {
				continue
			}
			seen[mc] = true
			if neoForgeStable(b) {
				stable[mc] = true
			}
		}
		for mc := range seen {
			out = append(out, Version{ID: mc, Stable: stableVersion(mc) && stable[mc]})
		}
	case Custom:
		return nil, nil
	default:
		return nil, fmt.Errorf("%w: unknown type %q", ErrInvalid, t)
	}
	out = filterOffered(t, out)
	sortVersionsDesc(out)
	return out, nil
}

func filterOffered(t Type, in []Version) []Version {
	out := in[:0]
	for _, v := range in {
		if versionPattern(v.ID) && offered(t, v.ID) {
			out = append(out, v)
		}
	}
	return out
}

func sortVersionsDesc(vs []Version) {
	sort.SliceStable(vs, func(i, j int) bool {
		a, aok := parseMC(vs[i].ID)
		b, bok := parseMC(vs[j].ID)
		if !aok || !bok {
			return aok && !bok
		}
		if c := a.cmp(b); c != 0 {
			return c > 0
		}
		if a.pre != b.pre {
			return !a.pre
		}
		return vs[i].ID > vs[j].ID
	})
}

func (r *Resolver) Builds(ctx context.Context, t Type, mc string) ([]Build, error) {
	if !versionPattern(mc) {
		return nil, fmt.Errorf("%w: invalid minecraft version %q", ErrInvalid, mc)
	}
	var out []Build
	switch t {
	case Paper:
		var builds []struct {
			ID      int    `json:"id"`
			Channel string `json:"channel"`
		}
		if err := r.getJSON(ctx, fmt.Sprintf("%s/v3/projects/paper/versions/%s/builds", r.url(r.Fill, FillURL), url.PathEscape(mc)), &builds); err != nil {
			return nil, err
		}
		sort.Slice(builds, func(i, j int) bool { return builds[i].ID > builds[j].ID })
		for _, b := range builds {
			out = append(out, Build{ID: strconv.Itoa(b.ID), Stable: b.Channel == "STABLE"})
		}
	case Purpur:
		var v struct {
			Builds struct {
				Latest string   `json:"latest"`
				All    []string `json:"all"`
			} `json:"builds"`
		}
		if err := r.getJSON(ctx, fmt.Sprintf("%s/v2/purpur/%s", r.url(r.Purpur, PurpurURL), url.PathEscape(mc)), &v); err != nil {
			return nil, err
		}
		for i := len(v.Builds.All) - 1; i >= 0; i-- {
			out = append(out, Build{ID: v.Builds.All[i], Stable: true})
		}
	case Fabric:
		var loaders []struct {
			Loader struct {
				Version string `json:"version"`
				Stable  bool   `json:"stable"`
			} `json:"loader"`
		}
		if err := r.getJSON(ctx, fmt.Sprintf("%s/v2/versions/loader/%s", r.url(r.FabricMeta, FabricMetaURL), url.PathEscape(mc)), &loaders); err != nil {
			return nil, err
		}
		for _, l := range loaders {
			out = append(out, Build{ID: l.Loader.Version, Stable: l.Loader.Stable})
		}
		sort.SliceStable(out, func(i, j int) bool { return semverLess(out[j].ID, out[i].ID) })
	case Quilt:
		var loaders []struct {
			Loader struct {
				Version string `json:"version"`
			} `json:"loader"`
		}
		if err := r.getJSON(ctx, fmt.Sprintf("%s/v3/versions/loader/%s", r.url(r.QuiltMeta, QuiltMetaURL), url.PathEscape(mc)), &loaders); err != nil {
			return nil, err
		}
		for _, l := range loaders {
			out = append(out, Build{ID: l.Loader.Version, Stable: !prereleaseTag(l.Loader.Version)})
		}
		sort.SliceStable(out, func(i, j int) bool { return semverLess(out[j].ID, out[i].ID) })
	case Forge:
		index, err := r.forgeIndex(ctx)
		if err != nil {
			return nil, err
		}
		promos, err := r.forgePromotions(ctx)
		if err != nil {
			return nil, err
		}
		ids := index[mc]
		for i := len(ids) - 1; i >= 0; i-- {
			b, ok := strings.CutPrefix(ids[i], mc+"-")
			if !ok {
				continue
			}
			out = append(out, Build{ID: b, Stable: true})
		}
		pick := promos[mc+"-recommended"]
		if pick == "" {
			pick = promos[mc+"-latest"]
		}
		if pick != "" {
			for i := range out {
				if out[i].ID == pick {
					out[i].Recommended = true
					return out, nil
				}
			}
		}
	case NeoForge:
		builds, err := r.neoForgeVersions(ctx)
		if err != nil {
			return nil, err
		}
		for _, b := range builds {
			if m, ok := neoForgeMinecraft(b); ok && m == mc {
				out = append(out, Build{ID: b, Stable: neoForgeStable(b)})
			}
		}
		sort.SliceStable(out, func(i, j int) bool { return semverLess(out[j].ID, out[i].ID) })
	default:
		return nil, fmt.Errorf("%w: %s has no builds", ErrInvalid, t)
	}
	if len(out) == 0 {
		return nil, &NotFoundError{What: fmt.Sprintf("%s builds for Minecraft %s", t, mc)}
	}
	for i := range out {
		if out[i].Stable {
			out[i].Recommended = true
			return out, nil
		}
	}
	out[0].Recommended = true
	return out, nil
}

func (r *Resolver) RecommendedBuild(ctx context.Context, t Type, mc string) (string, error) {
	builds, err := r.Builds(ctx, t, mc)
	if err != nil {
		return "", err
	}
	for _, b := range builds {
		if b.Recommended {
			return b.ID, nil
		}
	}
	return builds[0].ID, nil
}

func (r *Resolver) HasVersion(ctx context.Context, t Type, mc string) (bool, error) {
	vs, err := r.Versions(ctx, t)
	if err != nil {
		return false, err
	}
	for _, v := range vs {
		if v.ID == mc {
			return true, nil
		}
	}
	return false, nil
}

func (r *Resolver) HasBuild(ctx context.Context, t Type, mc, b string) (bool, error) {
	builds, err := r.Builds(ctx, t, mc)
	if err != nil {
		var nf *NotFoundError
		if asNotFound(err, &nf) {
			return false, nil
		}
		return false, err
	}
	for _, x := range builds {
		if x.ID == b {
			return true, nil
		}
	}
	return false, nil
}

func (r *Resolver) RequiredJava(ctx context.Context, t Type, mc string) (int, error) {
	if t == Custom {
		return 0, nil
	}
	if t == Paper {
		var v struct {
			Version struct {
				Java struct {
					Version struct {
						Minimum int `json:"minimum"`
					} `json:"version"`
				} `json:"java"`
			} `json:"version"`
		}
		if err := r.getJSON(ctx, fmt.Sprintf("%s/v3/projects/paper/versions/%s", r.url(r.Fill, FillURL), url.PathEscape(mc)), &v); err == nil && v.Version.Java.Version.Minimum > 0 {
			return v.Version.Java.Version.Minimum, nil
		}
	}
	mv, err := r.mojangVersion(ctx, mc)
	if err != nil {
		return 0, err
	}
	if mv.JavaVersion.MajorVersion > 0 {
		return mv.JavaVersion.MajorVersion, nil
	}
	return 8, nil
}

func prereleaseTag(v string) bool {
	l := strings.ToLower(v)
	for _, tag := range []string{"alpha", "beta", "pre", "rc", "snapshot"} {
		if strings.Contains(l, tag) {
			return true
		}
	}
	return false
}

type mojangManifest struct {
	Versions []struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		URL  string `json:"url"`
		SHA1 string `json:"sha1"`
	} `json:"versions"`
}

type mojangVersionJSON struct {
	ID          string `json:"id"`
	JavaVersion struct {
		MajorVersion int `json:"majorVersion"`
	} `json:"javaVersion"`
	Downloads struct {
		Server *struct {
			SHA1 string `json:"sha1"`
			Size int64  `json:"size"`
			URL  string `json:"url"`
		} `json:"server"`
	} `json:"downloads"`
}

func (r *Resolver) mojangManifest(ctx context.Context) (*mojangManifest, error) {
	var m mojangManifest
	if err := r.getJSON(ctx, r.url(r.MojangManifest, MojangManifestURL), &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func (r *Resolver) mojangVersion(ctx context.Context, mc string) (*mojangVersionJSON, error) {
	m, err := r.mojangManifest(ctx)
	if err != nil {
		return nil, err
	}
	for _, v := range m.Versions {
		if v.ID != mc {
			continue
		}
		body, err := r.get(ctx, v.URL)
		if err != nil {
			return nil, err
		}
		if v.SHA1 != "" {
			want, err := ParseDigest(SHA1, v.SHA1)
			if err != nil {
				return nil, &UpstreamError{URL: v.URL, Err: err}
			}
			if !bytesMatch(body, want) {
				return nil, &UpstreamError{URL: v.URL, Err: ErrDigestMismatch}
			}
		}
		var out mojangVersionJSON
		if err := jsonUnmarshal(body, &out); err != nil {
			return nil, &UpstreamError{URL: v.URL, Err: err}
		}
		return &out, nil
	}
	return nil, &NotFoundError{What: "Minecraft version " + mc}
}

func (r *Resolver) forgeIndex(ctx context.Context) (map[string][]string, error) {
	var index map[string][]string
	err := r.getJSON(ctx, r.url(r.ForgeFiles, ForgeFilesURL)+"/net/minecraftforge/forge/maven-metadata.json", &index)
	return index, err
}

func (r *Resolver) forgePromotions(ctx context.Context) (map[string]string, error) {
	var p struct {
		Promos map[string]string `json:"promos"`
	}
	err := r.getJSON(ctx, r.url(r.ForgeFiles, ForgeFilesURL)+"/net/minecraftforge/forge/promotions_slim.json", &p)
	return p.Promos, err
}

func (r *Resolver) neoForgeVersions(ctx context.Context) ([]string, error) {
	var v struct {
		Versions []string `json:"versions"`
	}
	err := r.getJSON(ctx, r.url(r.NeoForgeMaven, NeoForgeMavenURL)+"/api/maven/versions/releases/net/neoforged/neoforge", &v)
	return v.Versions, err
}

// NeoForge versions map 21.4.x to Minecraft 1.21.4; year-based versions preserve the full game version and omit a zero patch.
func neoForgeMinecraft(v string) (string, bool) {
	base, _, _ := strings.Cut(v, "-")
	f := strings.Split(base, ".")
	nums := make([]int, len(f))
	for i, s := range f {
		n, err := strconv.Atoi(s)
		if err != nil {
			return "", false
		}
		nums[i] = n
	}
	switch {
	case len(nums) == 3 && nums[0] >= 20 && nums[0] < 26:
		if nums[1] == 0 {
			return fmt.Sprintf("1.%d", nums[0]), true
		}
		return fmt.Sprintf("1.%d.%d", nums[0], nums[1]), true
	case len(nums) == 4 && nums[0] >= 26:
		if nums[2] == 0 {
			return fmt.Sprintf("%d.%d", nums[0], nums[1]), true
		}
		return fmt.Sprintf("%d.%d.%d", nums[0], nums[1], nums[2]), true
	}
	return "", false
}

func neoForgeStable(v string) bool { return !prereleaseTag(v) }

func numericParts(v string) []int {
	base, _, _ := strings.Cut(v, "-")
	var out []int
	for _, s := range strings.Split(base, ".") {
		n, err := strconv.Atoi(s)
		if err != nil {
			break
		}
		out = append(out, n)
	}
	return out
}

func semverLess(a, b string) bool {
	ab, apre, _ := strings.Cut(a, "-")
	bb, bpre, _ := strings.Cut(b, "-")
	pa, pb := numericParts(ab), numericParts(bb)
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			return x < y
		}
	}
	switch {
	case apre == bpre:
		return false
	case apre == "":
		return false
	case bpre == "":
		return true
	}
	ai, bi := strings.Split(apre, "."), strings.Split(bpre, ".")
	for i := 0; i < len(ai) && i < len(bi); i++ {
		if ai[i] == bi[i] {
			continue
		}
		x, xerr := strconv.Atoi(ai[i])
		y, yerr := strconv.Atoi(bi[i])
		if xerr == nil && yerr == nil {
			return x < y
		}
		return ai[i] < bi[i]
	}
	return len(ai) < len(bi)
}
