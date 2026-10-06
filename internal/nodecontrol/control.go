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
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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
}

func (r Request) Validate() error {
	if !r.ConfirmMaintenance {
		return errors.New("maintenance impact confirmation is required")
	}
	switch r.Action {
	case "join", "approve":
		if !validNodeName(r.Name) || !targetPattern.MatchString(r.SSHTarget) {
			return errors.New("invalid worker name or SSH target")
		}
	case "enable":
	default:
		return errors.New("unsupported node operation")
	}
	if r.Action != "approve" {
		if a, err := netip.ParseAddr(r.ExternalIP); err != nil || a.IsUnspecified() || a.IsLoopback() || a.IsMulticast() || a.Zone() != "" {
			return errors.New("a fixed node IP is required")
		}
	}
	for _, peer := range r.Peers {
		p, err := netip.ParsePrefix(peer)
		if err != nil || p.Bits() != p.Addr().BitLen() || p.Addr().IsLoopback() || p.Addr().IsMulticast() || p.Addr().IsUnspecified() {
			return errors.New("peer addresses must use exact /32 or /128 prefixes")
		}
	}
	if len(r.Peers) > 100 {
		return errors.New("too many peer addresses")
	}
	return nil
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
}

type Executor func(context.Context, Request, func(string) error, io.Writer) error

type Manager struct {
	dir     string
	execute Executor
	mu      sync.Mutex
	tasks   map[string]Task
	active  string
	ctx     context.Context
	wg      sync.WaitGroup
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
	t := Task{ID: hex.EncodeToString(id[:]), Request: r, Actor: actor, State: "running", Stage: "preflight", StartedAt: time.Now().UTC()}
	if err := m.persist(t); err != nil {
		return Task{}, err
	}
	m.tasks[t.ID] = t
	m.active = t.ID
	m.wg.Add(1)
	go m.run(t)
	return t, nil
}
func (m *Manager) run(t Task) {
	defer m.wg.Done()
	ctx, cancel := context.WithTimeout(m.ctx, 45*time.Minute)
	defer cancel()
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
		err = m.execute(ctx, t.Request, stage, &boundedLog{file: log})
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
	if err != nil {
		t.State = "failed"
		t.Error = err.Error()
		if errors.Is(err, context.DeadlineExceeded) {
			t.Error = "Host operation exceeded the 45-minute execution limit. Verify the host state before retrying."
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
	return mux
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
