package registrygate

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// IndexPathPrefix serves the manifest index of one repository:
//
//	GET /felis/manifests/<repo>  {"revisions":[{"digest":…,"pushed":…,"children":[…]}],"tags":{"<tag>":"<digest>"}}
//
// The registry API can list a repository's tags but not its manifests, so a
// manifest a tag moved off (every rebuild of :demo or :latest leaves one) is
// invisible to it, while it still holds every layer it names. felis-api's pruner
// needs the full set to decide which to delete; the gate reads it off the data
// volume it mounts read-only, from the filesystem driver's layout that registry
// 2.x and 3.x share. "pushed" is the modification time of the revision link, which
// the registry rewrites on every push of that manifest.
//
// Anonymous like every read: a digest list says no more than tags/list and
// _catalog already do.
const IndexPathPrefix = "/felis/manifests/"

// Index is the manifest index of one repository.
type Index struct {
	Revisions []Revision        `json:"revisions"`
	Tags      map[string]string `json:"tags"`
}

// Revision is one manifest stored in a repository.
type Revision struct {
	Digest string    `json:"digest"`
	Pushed time.Time `json:"pushed"`
	// Children lists the manifests an image index (or Docker manifest list) names:
	// one per platform, plus BuildKit's attestation manifests. Each is a revision
	// of its own, untagged and never spelled in an image ref, yet a pull of the
	// index fetches them, so whoever keeps the index must keep them too. Empty for
	// a single-platform manifest.
	Children []string `json:"children,omitempty"`
}

// maxManifestBytes caps how much of a revision's blob is read to find its
// children. It is the registry's own limit on a pushed manifest.
const maxManifestBytes = 4 << 20

// manifestChildren reads a manifest blob off the filesystem driver's layout and
// returns the digests it names when it is an index. A blob that is missing,
// oversized or unparsable names nothing: a pull of it fails already, and the
// pruner then treats it like any other revision.
func manifestChildren(root, hex string) []string {
	f, err := os.Open(filepath.Join(root, "docker", "registry", "v2", "blobs", "sha256", hex[:2], hex, "data"))
	if err != nil {
		return nil
	}
	defer f.Close()
	// The manifests array is what makes an index, whatever mediaType says (an OCI
	// index may omit it); a single-platform manifest has layers and no manifests.
	var m struct {
		Manifests []struct {
			Digest string `json:"digest"`
		} `json:"manifests"`
	}
	if err := json.NewDecoder(io.LimitReader(f, maxManifestBytes)).Decode(&m); err != nil {
		return nil
	}
	var out []string
	for _, c := range m.Manifests {
		if digestRE.MatchString(c.Digest) {
			out = append(out, c.Digest)
		}
	}
	return out
}

// repoNameRE is the distribution reference grammar for a repository path. Every
// component starts and ends alphanumeric, so no match can hold "." or "..".
var repoNameRE = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|[-]*)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|[-]*)[a-z0-9]+)*)*$`)

var hexDigestRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (g *Gate) serveIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")
		return
	}
	if g.DataDir == "" {
		writeError(w, http.StatusNotFound, "UNSUPPORTED", "this gate does not mount the registry data")
		return
	}
	repo := strings.TrimPrefix(r.URL.Path, IndexPathPrefix)
	if r.URL.RawPath != "" || !repoNameRE.MatchString(repo) {
		writeError(w, http.StatusBadRequest, "NAME_INVALID", "invalid repository name")
		return
	}
	idx, err := ReadIndex(g.DataDir, repo)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "UNKNOWN", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(idx)
}

// ReadIndex reads repo's manifests and tags from a registry filesystem root. A
// repository that does not exist has an empty index.
func ReadIndex(root, repo string) (*Index, error) {
	if !repoNameRE.MatchString(repo) {
		return nil, errors.New("invalid repository name")
	}
	base := filepath.Join(root, "docker", "registry", "v2", "repositories", filepath.FromSlash(repo), "_manifests")
	idx := &Index{Revisions: []Revision{}, Tags: map[string]string{}}

	revDir := filepath.Join(base, "revisions", "sha256")
	entries, err := os.ReadDir(revDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() || !hexDigestRE.MatchString(e.Name()) {
			continue
		}
		// A deleted manifest keeps its directory and loses its link.
		st, err := os.Stat(filepath.Join(revDir, e.Name(), "link"))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		idx.Revisions = append(idx.Revisions, Revision{Digest: "sha256:" + e.Name(), Pushed: st.ModTime().UTC(),
			Children: manifestChildren(root, e.Name())})
	}
	sort.Slice(idx.Revisions, func(i, j int) bool { return idx.Revisions[i].Digest < idx.Revisions[j].Digest })

	tagDir := filepath.Join(base, "tags")
	tags, err := os.ReadDir(tagDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for _, e := range tags {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(tagDir, e.Name(), "current", "link"))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if d := strings.TrimSpace(string(b)); digestRE.MatchString(d) {
			idx.Tags[e.Name()] = d
		}
	}
	return idx, nil
}
