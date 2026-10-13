// Package nodecontrol exposes fixed host operations over a local Unix socket.
package nodecontrol

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const Socket = "/run/felis-node-control/control.sock"
const MaxLog = 64 << 10

var ErrBusy = errors.New("a node operation is already running")
var ErrNotFound = errors.New("node operation not found")
var namePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
var targetPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.@:-]{0,252}$`)
var idPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

func validNodeName(name string) bool {
	if len(name) > 63 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if !namePattern.MatchString(label) {
			return false
		}
	}
	return true
}

type Request struct {
	Action             string   `json:"action"`
	Name               string   `json:"name,omitempty"`
	SSHTarget          string   `json:"sshTarget,omitempty"`
	ExternalIP         string   `json:"externalIP,omitempty"`
	Peers              []string `json:"peers,omitempty"`
	ConfirmMaintenance bool     `json:"confirmMaintenance"`
	// Params are the action's own inputs (actions.go), kept with the task.
	Params map[string]string `json:"params,omitempty"`
	// Secrets reach the executor only: a task never stores or reports them, so a retry
	// asks for them again.
	Secrets map[string]string `json:"secrets,omitempty"`
}

type Task struct {
	ID         string     `json:"id"`
	Request    Request    `json:"request"`
	Actor      string     `json:"actor"`
	State      string     `json:"state"`
	Stage      string     `json:"stage"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	Error      string     `json:"error,omitempty"`
	Log        string     `json:"log,omitempty"`
	// Result is what the action reports back as data (findings, a check's answer),
	// set through SetResult; Artifact is a file it produced for download (Artifact).
	Result   json.RawMessage `json:"result,omitempty"`
	Artifact string          `json:"artifact,omitempty"`
}

// MaxResult bounds a task's Result.
const MaxResult = 256 << 10

type Executor func(context.Context, Request, func(string) error, io.Writer) error

type outcomeKey struct{}

// outcome collects what the executor reports besides its log.
type outcome struct {
	mu       sync.Mutex
	result   json.RawMessage
	artifact string
}

// SetResult records v, as JSON, as the running task's Result.
func SetResult(ctx context.Context, v any) error {
	o, ok := ctx.Value(outcomeKey{}).(*outcome)
	if !ok {
		return errors.New("no host task is running")
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(raw) > MaxResult {
		return errors.New("the task result is too large")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.result = raw
	return nil
}

// SetArtifact records path, a file under one of the manager's artifact directories,
// as the running task's download.
func SetArtifact(ctx context.Context, path string) error {
	o, ok := ctx.Value(outcomeKey{}).(*outcome)
	if !ok {
		return errors.New("no host task is running")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.artifact = path
	return nil
}

type Manager struct {
	dir     string
	execute Executor
	mu      sync.Mutex
	tasks   map[string]Task
	active  string
	ctx     context.Context
	wg      sync.WaitGroup
	// ArtifactDirs are where a task's download may live; a path elsewhere is not served.
	ArtifactDirs []string
	// Host answers GET /host, the platform's state at a glance; Quiet holds the
	// watchdog's mail until the given time (the zero time lifts the hold). Nil hooks
	// answer 404.
	Host  func(context.Context) (any, error)
	Quiet func(time.Time) error
	// OffsiteKey reads the key that seals the off-site copies, for the Owner to keep
	// elsewhere; it is answered, never stored with a task.
	OffsiteKey func() (string, error)
}

func Open(ctx context.Context, dir string, execute Executor) (*Manager, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	m := &Manager{dir: dir, execute: execute, tasks: map[string]Task{}, ctx: ctx}
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var t Task
		if err = json.Unmarshal(raw, &t); err != nil {
			return nil, fmt.Errorf("read node task: %w", err)
		}
		if !idPattern.MatchString(t.ID) || filepath.Base(path) != t.ID+".json" {
			return nil, errors.New("invalid stored task ID")
		}
		if t.State == "running" {
			t.State, t.Error = "failed", "Host execution service restarted. Verify the host state before retrying."
			now := time.Now().UTC()
			t.FinishedAt = &now
			if err = m.persist(t); err != nil {
				return nil, err
			}
		}
		m.tasks[t.ID] = t
	}
	return m, nil
}
func (m *Manager) persist(t Task) error {
	t.Log = ""
	raw, err := json.Marshal(t)
	if err != nil {
		return err
	}
	path := filepath.Join(m.dir, t.ID+".json")
	f, err := os.OpenFile(path+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(path+".tmp", path); err != nil {
		return err
	}
	d, err := os.Open(m.dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func (m *Manager) List() []Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Task, 0, len(m.tasks))
	for _, t := range m.tasks {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	if len(out) > 100 {
		out = out[:100]
	}
	return out
}
func (m *Manager) Get(id string) (Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return Task{}, ErrNotFound
	}
	f, err := os.Open(filepath.Join(m.dir, id+".log"))
	if errors.Is(err, os.ErrNotExist) {
		return t, nil
	}
	if err != nil {
		return t, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return t, err
	}
	if info.Size() > MaxLog {
		if _, err = f.Seek(-MaxLog, io.SeekEnd); err != nil {
			return t, err
		}
	}
	raw, err := io.ReadAll(f)
	t.Log = string(raw)
	return t, err
}
func (m *Manager) Start(r Request, actor string) (Task, error) {
	if err := r.Validate(); err != nil {
		return Task{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active != "" {
		return Task{}, ErrBusy
	}
	if err := m.ctx.Err(); err != nil {
		return Task{}, err
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return Task{}, err
	}
	secrets := r.Secrets
	r.Secrets = nil
	t := Task{ID: hex.EncodeToString(id[:]), Request: r, Actor: actor, State: "running", Stage: "preflight", StartedAt: time.Now().UTC()}
	if err := m.persist(t); err != nil {
		return Task{}, err
	}
	m.tasks[t.ID] = t
	m.active = t.ID
	m.wg.Add(1)
	go m.run(t, secrets)
	return t, nil
}
func (m *Manager) run(t Task, secrets map[string]string) {
	defer m.wg.Done()
	limit := timeoutFor(t.Request.Action)
	o := &outcome{}
	ctx, cancel := context.WithTimeout(context.WithValue(m.ctx, outcomeKey{}, o), limit)
	defer cancel()
	req := t.Request
	req.Secrets = secrets
	log, err := os.OpenFile(filepath.Join(m.dir, t.ID+".log"), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
	if err == nil {
		stage := func(s string) error {
			m.mu.Lock()
			defer m.mu.Unlock()
			t.Stage = s
			if err := m.persist(t); err != nil {
				return err
			}
			m.tasks[t.ID] = t
			_, err := fmt.Fprintln(log, "[felis]", s)
			return err
		}
		err = m.execute(ctx, req, stage, &boundedLog{file: log})
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		if closeErr := log.Close(); err == nil {
			err = closeErr
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	t.FinishedAt = &now
	t.State = "succeeded"
	o.mu.Lock()
	t.Result, t.Artifact = o.result, o.artifact
	o.mu.Unlock()
	if err != nil {
		t.State = "failed"
		t.Error = err.Error()
		if errors.Is(err, context.DeadlineExceeded) {
			t.Error = fmt.Sprintf("Host operation exceeded the %s execution limit. Verify the host state before retrying.", limitText(limit))
		}
		if errors.Is(err, context.Canceled) {
			t.Error = "Host operation was interrupted. Verify the host state before retrying."
		}
	}
	if persistErr := m.persist(t); persistErr != nil {
		t.State = "failed"
		t.Error = "Task result persistence failed: " + persistErr.Error()
	}
	m.tasks[t.ID] = t
	m.active = ""
	for len(m.tasks) > 100 {
		oldest := ""
		for id, candidate := range m.tasks {
			if id == t.ID {
				continue
			}
			if oldest == "" || candidate.StartedAt.Before(m.tasks[oldest].StartedAt) {
				oldest = id
			}
		}
		if oldest != "" {
			if err := os.Remove(filepath.Join(m.dir, oldest+".json")); err == nil {
				os.Remove(filepath.Join(m.dir, oldest+".log"))
				delete(m.tasks, oldest)
			} else {
				break
			}
		}
	}
}
func (m *Manager) Wait() { m.wg.Wait() }

// Handler is reachable only through the root-owned socket mounted into felis-api.
func (m *Manager) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /tasks", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(m.List()) })
	mux.HandleFunc("GET /tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		t, err := m.Get(r.PathValue("id"))
		if err != nil {
			http.Error(w, err.Error(), 404)
			return
		}
		json.NewEncoder(w).Encode(t)
	})
	mux.HandleFunc("POST /tasks", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Request Request `json:"request"`
			Actor   string  `json:"actor"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			http.Error(w, "invalid task request", 400)
			return
		}
		if err := dec.Decode(&struct{}{}); err != io.EOF {
			http.Error(w, "invalid trailing task data", 400)
			return
		}
		if err := body.Request.Validate(); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		t, err := m.Start(body.Request, body.Actor)
		if err != nil {
			code := 500
			if errors.Is(err, ErrBusy) {
				code = 409
			}
			http.Error(w, err.Error(), code)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(t)
	})
	mux.HandleFunc("GET /tasks/{id}/artifact", m.serveArtifact)
	mux.HandleFunc("GET /host", func(w http.ResponseWriter, r *http.Request) {
		if m.Host == nil {
			http.Error(w, "no host overview", 404)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
		defer cancel()
		v, err := m.Host(ctx)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		json.NewEncoder(w).Encode(v)
	})
	mux.HandleFunc("GET /host/offsite-key", func(w http.ResponseWriter, r *http.Request) {
		if m.OffsiteKey == nil {
			http.Error(w, "no off-site key", 404)
			return
		}
		key, err := m.OffsiteKey()
		if err != nil {
			http.Error(w, err.Error(), 404)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(map[string]string{"key": key})
	})
	mux.HandleFunc("POST /host/quiet", func(w http.ResponseWriter, r *http.Request) {
		if m.Quiet == nil {
			http.Error(w, "no watchdog hold", 404)
			return
		}
		var body struct {
			Until time.Time `json:"until"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			http.Error(w, "invalid hold request", 400)
			return
		}
		if !body.Until.IsZero() && (body.Until.Before(time.Now()) || body.Until.After(time.Now().Add(7*24*time.Hour))) {
			http.Error(w, "a hold ends within the next seven days", 400)
			return
		}
		if err := m.Quiet(body.Until); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

// serveArtifact streams a finished task's download. The path comes from the task the
// executor recorded, and is served only from an artifact directory, as a regular file.
func (m *Manager) serveArtifact(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	t, ok := m.tasks[r.PathValue("id")]
	m.mu.Unlock()
	if !ok || t.State != "succeeded" || t.Artifact == "" {
		http.Error(w, "no download for this task", 404)
		return
	}
	path := filepath.Clean(t.Artifact)
	inside := false
	for _, dir := range m.ArtifactDirs {
		if rel, err := filepath.Rel(filepath.Clean(dir), path); err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !strings.Contains(rel, string(filepath.Separator)) {
			inside = true
		}
	}
	info, err := os.Lstat(path)
	if !inside || err != nil || !info.Mode().IsRegular() {
		http.Error(w, "no download for this task", 404)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "no download for this task", 404)
		return
	}
	defer f.Close()
	// A large bundle outlasts the server's write timeout on a slow browser.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Minute))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("X-Felis-Filename", filepath.Base(path))
	_, _ = io.Copy(w, f)
}

type Client struct{ http *http.Client }

func NewClient(socket string) *Client {
	return &Client{http: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}}
}
func (c *Client) call(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = strings.NewReader(string(raw))
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://node-control"+path, reader)
	if err != nil {
		return err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode == 409 {
		return ErrBusy
	}
	if res.StatusCode == 404 {
		return ErrNotFound
	}
	if res.StatusCode >= 400 {
		return fmt.Errorf("host service returned HTTP %d", res.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(out)
}
func (c *Client) List(ctx context.Context) ([]Task, error) {
	var out []Task
	err := c.call(ctx, "GET", "/tasks", nil, &out)
	return out, err
}
func (c *Client) Get(ctx context.Context, id string) (Task, error) {
	if !idPattern.MatchString(id) {
		return Task{}, ErrNotFound
	}
	var out Task
	err := c.call(ctx, "GET", "/tasks/"+id, nil, &out)
	return out, err
}
func (c *Client) Start(ctx context.Context, r Request, actor string) (Task, error) {
	var out Task
	err := c.call(ctx, "POST", "/tasks", map[string]any{"request": r, "actor": actor}, &out)
	return out, err
}

// Host reads the platform's state at a glance, as the host service reports it.
func (c *Client) Host(ctx context.Context) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.call(ctx, "GET", "/host", nil, &out)
	return out, err
}

// OffsiteKey reads the key that seals the off-site copies.
func (c *Client) OffsiteKey(ctx context.Context) (string, error) {
	var out struct {
		Key string `json:"key"`
	}
	err := c.call(ctx, "GET", "/host/offsite-key", nil, &out)
	return out.Key, err
}

// Quiet holds the watchdog's mail until until; the zero time lifts the hold.
func (c *Client) Quiet(ctx context.Context, until time.Time) error {
	raw, err := json.Marshal(map[string]time.Time{"until": until})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", "http://node-control/host/quiet", strings.NewReader(string(raw)))
	if err != nil {
		return err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		return fmt.Errorf("host service returned HTTP %d", res.StatusCode)
	}
	return nil
}

// Artifact opens a finished task's download: its body, size and file name. The
// caller closes the body. It is not bound by the client's request timeout, only ctx.
func (c *Client) Artifact(ctx context.Context, id string) (io.ReadCloser, int64, string, error) {
	if !idPattern.MatchString(id) {
		return nil, 0, "", ErrNotFound
	}
	req, err := http.NewRequestWithContext(ctx, "GET", "http://node-control/tasks/"+id+"/artifact", nil)
	if err != nil {
		return nil, 0, "", err
	}
	stream := &http.Client{Transport: c.http.Transport}
	res, err := stream.Do(req)
	if err != nil {
		return nil, 0, "", err
	}
	if res.StatusCode == 404 {
		res.Body.Close()
		return nil, 0, "", ErrNotFound
	}
	if res.StatusCode >= 400 {
		res.Body.Close()
		return nil, 0, "", fmt.Errorf("host service returned HTTP %d", res.StatusCode)
	}
	return res.Body, res.ContentLength, filepath.Base(res.Header.Get("X-Felis-Filename")), nil
}

// Bound disk use while retaining the most recent output of long builds.
type boundedLog struct {
	file *os.File
	mu   sync.Mutex
}

func (w *boundedLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	const limit = 4 << 20
	info, err := w.file.Stat()
	if err != nil {
		return 0, err
	}
	if info.Size()+int64(len(p)) > limit {
		n := int64(MaxLog)
		if info.Size() < n {
			n = info.Size()
		}
		tail := make([]byte, n)
		if _, err = w.file.ReadAt(tail, info.Size()-n); err != nil {
			return 0, err
		}
		if err = w.file.Truncate(0); err != nil {
			return 0, err
		}
		if _, err = w.file.Write(tail); err != nil {
			return 0, err
		}
	}
	original := len(p)
	if len(p) > limit {
		p = p[len(p)-limit:]
	}
	_, err = w.file.Write(p)
	if err != nil {
		return 0, err
	}
	return original, nil
}

// limitText reads a task's time limit as the error message words it: "45-minute",
// "12-hour".
func limitText(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%d-hour", d/time.Hour)
	}
	return fmt.Sprintf("%d-minute", d/time.Minute)
}
