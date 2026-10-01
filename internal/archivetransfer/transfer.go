// Package archivetransfer serves archive bytes on A. It has no Kubernetes or database access.
package archivetransfer

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"felis.lolicon.best/internal/backup"
	"felis.lolicon.best/internal/naming"
	"felis.lolicon.best/internal/worldexport"
)

const TokenEnv = "FELIS_ARCHIVE_TOKEN"
const KeyEnv = "FELIS_ARCHIVE_KEY"
const LabelPending = "felis.lolicon.best/archive-pending"
const Annotation = "felis.lolicon.best/archive-transfer"
const DefaultLimit int64 = 100 << 30

var idRE = regexp.MustCompile(`^[a-f0-9]{32}$`)

// Ticket names exactly one operation, server and archive. Only A holds the signing key.
type Ticket struct {
	ID      string    `json:"id"`
	Server  string    `json:"server"`
	Method  string    `json:"method"`
	Ref     string    `json:"ref"`
	SHA256  string    `json:"sha256,omitempty"`
	Limit   int64     `json:"limit"`
	Expires time.Time `json:"expires"`
}

type Receipt struct {
	Ref    string `json:"ref"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type Client struct {
	URL, Root, Key string
	Limit          int64
}

func ID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func (c Client) Issue(server, method, ref, sum string, ttl time.Duration) (Ticket, string, string, error) {
	if len(c.Key) < 32 || c.URL == "" || ttl <= 0 || ttl > 24*time.Hour {
		return Ticket{}, "", "", errors.New("archive transport is not configured or ticket lifetime is invalid")
	}
	id := ID()
	if method == http.MethodPut {
		ref = filepath.Join(c.Root, fmt.Sprintf("%s-%d.tar.gz", server, time.Now().UnixNano()))
	}
	limit := c.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	t := Ticket{ID: id, Server: server, Method: method, Ref: ref, SHA256: sum, Limit: limit, Expires: time.Now().Add(ttl)}
	if err := validate(t, c.Root, limit); err != nil {
		return Ticket{}, "", "", err
	}
	raw, _ := json.Marshal(t)
	payload := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, []byte(c.Key))
	mac.Write([]byte(payload))
	token := payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return t, strings.TrimRight(c.URL, "/") + "/transfers/" + id, token, nil
}

func validate(t Ticket, root string, limit int64) error {
	if !idRE.MatchString(t.ID) || t.Limit <= 0 || t.Limit > limit {
		return errors.New("invalid transfer bounds")
	}
	if err := naming.ValidateSystemServerName(t.Server); err != nil {
		return err
	}
	if t.Method != http.MethodPut && t.Method != http.MethodGet {
		return errors.New("invalid transfer operation")
	}
	if !filepath.IsAbs(root) || filepath.Dir(t.Ref) != filepath.Clean(root) || !strings.HasSuffix(t.Ref, ".tar.gz") {
		return errors.New("archive must be directly inside the archive root")
	}
	if !strings.HasPrefix(filepath.Base(t.Ref), t.Server+"-") {
		return errors.New("archive belongs to another server")
	}
	if t.SHA256 != "" {
		b, err := hex.DecodeString(t.SHA256)
		if err != nil || len(b) != sha256.Size {
			return errors.New("invalid SHA-256")
		}
	}
	return nil
}

// Server journals consumption before IO, so process restarts cannot enable replay.
// A failure consumes the ticket too: the controller issues a fresh ticket on retry.
type Server struct {
	Root, Key string
	Limit     int64
	mu        sync.Mutex
	freeBytes func() (int64, error)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" && r.Method == http.MethodGet {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.URL.Path == "/archives" && r.Method == http.MethodDelete {
		if len(s.Key) < 32 || !hmac.Equal([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")), []byte(s.Key)) {
			http.Error(w, "unauthorized", 401)
			return
		}
		var q struct {
			Ref string `json:"ref"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&q) != nil || filepath.Dir(q.Ref) != filepath.Clean(s.Root) {
			http.Error(w, "bad ref", 400)
			return
		}
		if err := os.Remove(q.Ref); err != nil && !errors.Is(err, os.ErrNotExist) {
			http.Error(w, "archive delete failed", 500)
			return
		}
		if err := syncDir(s.Root); err != nil {
			http.Error(w, "archive delete commit failed", 500)
			return
		}
		w.WriteHeader(204)
		return
	}
	if r.URL.Path == "/inspect" && r.Method == http.MethodPost {
		if len(s.Key) < 32 || !hmac.Equal([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")), []byte(s.Key)) {
			http.Error(w, "unauthorized", 401)
			return
		}
		var q struct {
			Ref string `json:"ref"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&q) != nil || filepath.Dir(q.Ref) != filepath.Clean(s.Root) {
			http.Error(w, "bad archive ref", 400)
			return
		}
		local := &backup.TarLocal{BackupRoot: s.Root}
		sum, err := local.Verify(r.Context(), backup.ArchiveRef(q.Ref), "")
		if err != nil {
			http.Error(w, "archive is absent or corrupt", 422)
			return
		}
		st, err := os.Stat(q.Ref)
		if err != nil {
			http.Error(w, "archive absent", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Receipt{Ref: q.Ref, SHA256: sum, Size: st.Size()})
		return
	}
	if strings.HasPrefix(r.URL.Path, "/receipts/") && r.Method == http.MethodGet {
		if len(s.Key) < 32 || !hmac.Equal([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")), []byte(s.Key)) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/receipts/")
		if !idRE.MatchString(id) {
			http.NotFound(w, r)
			return
		}
		rec, err := s.receipt(r.Context(), id)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(rec)
		return
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	payload, signature, ok := strings.Cut(token, ".")
	mac := hmac.New(sha256.New, []byte(s.Key))
	mac.Write([]byte(payload))
	sig, err := base64.RawURLEncoding.DecodeString(signature)
	if !ok || err != nil || len(s.Key) < 32 || !hmac.Equal(sig, mac.Sum(nil)) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	var t Ticket
	limit := s.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	if err != nil || json.Unmarshal(raw, &t) != nil || validate(t, s.Root, limit) != nil || time.Now().After(t.Expires) || t.Expires.After(time.Now().Add(24*time.Hour)) || t.Method != r.Method || r.URL.Path != "/transfers/"+t.ID {
		http.Error(w, "invalid or expired transfer", http.StatusForbidden)
		return
	}
	if err := s.consume(t); err != nil {
		if errors.Is(err, os.ErrExist) {
			http.Error(w, "transfer already consumed", http.StatusConflict)
		} else {
			http.Error(w, "cannot journal transfer", http.StatusInsufficientStorage)
		}
		return
	}
	if t.Method == http.MethodGet {
		s.download(w, r, t)
		return
	}
	// Serialize uploads and disk checks to preserve a free-space reserve.
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.upload(r, t); err != nil {
		http.Error(w, "archive upload failed: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func (s *Server) consume(t Ticket) error {
	id := t.ID
	dir := filepath.Join(s.Root, ".transfers")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, id+".used"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(t)
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	cerr := f.Close()
	if err != nil {
		return err
	}
	if cerr != nil {
		return cerr
	}
	return syncDir(dir)
}

func (s *Server) upload(r *http.Request, t Ticket) error {
	available, err := s.availableBytes()
	if err != nil {
		return err
	}
	bound := min(t.Limit, available)
	if bound <= 0 {
		return backup.ErrNoRoom
	}
	tmp := filepath.Join(s.Root, "."+filepath.Base(t.Ref)+".partial")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(r.Body, bound+1))
	if err == nil && n > bound {
		err = errors.New("archive exceeds size or disk limit")
	}
	digest := "sha-256=:" + base64.StdEncoding.EncodeToString(hash.Sum(nil)) + ":"
	if err == nil && r.Trailer.Get(worldexport.DigestTrailer) != digest {
		err = errors.New("SHA-256 trailer is absent or incorrect")
	}
	sum := hex.EncodeToString(hash.Sum(nil))
	if err == nil && t.SHA256 != "" && t.SHA256 != sum {
		err = errors.New("SHA-256 does not match ticket")
	}
	if err == nil {
		err = f.Sync()
	}
	cerr := f.Close()
	if err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	local := &backup.TarLocal{BackupRoot: s.Root}
	if _, err = local.Verify(r.Context(), backup.ArchiveRef(tmp), sum); err != nil {
		return err
	}
	// Never overwrite a committed archive, including one whose receipt was interrupted.
	if err = os.Link(tmp, t.Ref); err != nil {
		return err
	}
	if err = os.Remove(tmp); err != nil {
		return err
	}
	if err = syncDir(s.Root); err != nil {
		return err
	}
	rec := Receipt{Ref: t.Ref, Size: n, SHA256: sum}
	raw, _ := json.Marshal(rec)
	journal := filepath.Join(s.Root, ".transfers", t.ID+".json")
	f, err = os.OpenFile(journal, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	cerr = f.Close()
	if err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return syncDir(filepath.Dir(journal))
}

func (s *Server) download(w http.ResponseWriter, r *http.Request, t Ticket) {
	root, err := os.OpenRoot(s.Root)
	if err != nil {
		http.Error(w, "archive unavailable", 503)
		return
	}
	defer root.Close()
	f, err := root.Open(filepath.Base(t.Ref))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > t.Limit {
		http.Error(w, "archive exceeds transfer bounds", 413)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Length", fmt.Sprint(st.Size()))
	io.Copy(w, f)
}

func (c Client) Receipt(ctx context.Context, id string) (Receipt, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.URL, "/")+"/receipts/"+id, nil)
	if err != nil {
		return Receipt{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	hc := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.Do(req)
	if err != nil {
		return Receipt{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Receipt{}, fmt.Errorf("archive receipt: %s", resp.Status)
	}
	var rec Receipt
	err = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&rec)
	return rec, err
}

// Fetch stages only the authorized archive and verifies its hash before extraction.
func Fetch(ctx context.Context, url, token, dest, want string, limit int64) error {
	if want == "" {
		return errors.New("download requires recorded SHA-256")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("archive download: %s", resp.Status)
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(resp.Body, limit+1))
	if err == nil && (n > limit || hex.EncodeToString(hash.Sum(nil)) != want) {
		err = errors.New("archive size or SHA-256 mismatch")
	}
	if err == nil {
		err = f.Sync()
	}
	cerr := f.Close()
	if err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dest)
	}
	return err
}

func (c Client) Inspect(ctx context.Context, ref string) (Receipt, error) {
	raw, _ := json.Marshal(map[string]string{"ref": ref})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.URL, "/")+"/inspect", strings.NewReader(string(raw)))
	if err != nil {
		return Receipt{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	hc := &http.Client{Timeout: 30 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.Do(req)
	if err != nil {
		return Receipt{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return Receipt{}, fmt.Errorf("archive inspect: %s", resp.Status)
	}
	var rec Receipt
	err = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&rec)
	return rec, err
}

func (c Client) Delete(ctx context.Context, ref backup.ArchiveRef) error {
	raw, _ := json.Marshal(map[string]string{"ref": string(ref)})
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, strings.TrimRight(c.URL, "/")+"/archives", strings.NewReader(string(raw)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	hc := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 204 && resp.StatusCode != 404 {
		return fmt.Errorf("archive delete: %s", resp.Status)
	}
	return nil
}

func (s *Server) receipt(ctx context.Context, id string) (Receipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.Root, ".transfers", id+".json")
	var rec Receipt
	if raw, err := os.ReadFile(path); err == nil && json.Unmarshal(raw, &rec) == nil {
		return rec, nil
	}
	raw, err := os.ReadFile(filepath.Join(s.Root, ".transfers", id+".used"))
	if err != nil {
		return rec, err
	}
	var t Ticket
	if err = json.Unmarshal(raw, &t); err != nil {
		return rec, err
	}
	if t.Method != http.MethodPut {
		return rec, errors.New("not an upload")
	}
	sum, err := (&backup.TarLocal{BackupRoot: s.Root}).Verify(ctx, backup.ArchiveRef(t.Ref), t.SHA256)
	if err != nil {
		return rec, err
	}
	st, err := os.Stat(t.Ref)
	if err != nil || st.Size() > t.Limit {
		return rec, errors.New("invalid committed archive")
	}
	rec = Receipt{Ref: t.Ref, Size: st.Size(), SHA256: sum}
	raw, _ = json.Marshal(rec)
	tmp := path + ".partial"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return rec, err
	}
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	cerr := f.Close()
	if err == nil {
		err = cerr
	}
	if err != nil {
		return rec, err
	}
	if err = os.Rename(tmp, path); err != nil {
		return rec, err
	}
	return rec, syncDir(filepath.Dir(path))
}

func (s *Server) availableBytes() (int64, error) {
	if s.freeBytes != nil {
		return s.freeBytes()
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(s.Root, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail)*int64(st.Bsize) - int64(float64(st.Blocks)*float64(st.Bsize)*backup.MinFreeAfter), nil
}

// Sweep keeps receipts for committed archives across arbitrarily long A outages.
// Expired failed transfers and downloads need no replay journal: signature expiry
// still rejects them. The extra day leaves no overlap with an active upload.
func (s *Server) Sweep(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := filepath.Join(s.Root, ".transfers")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".used") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return err
		}
		var ticket Ticket
		if json.Unmarshal(raw, &ticket) != nil || now.Before(ticket.Expires.Add(24*time.Hour)) {
			continue
		}
		if ticket.Method == http.MethodPut {
			if _, err := os.Stat(ticket.Ref); err == nil {
				continue
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		for _, name := range []string{e.Name(), ticket.ID + ".json", ticket.ID + ".json.partial"} {
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return syncDir(dir)
}
