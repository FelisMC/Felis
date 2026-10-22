package fileedit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Browse reuses Execute but independently refuses mutating commands from its peer.
func Browse(ctx context.Context, worldRoot, url, token string) error {
	if token == "" {
		return fmt.Errorf("fileedit: browser token is required")
	}
	client := &http.Client{Timeout: browserIdle + 30*time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	answer := BrowseAnswer{}
	for {
		body, err := json.Marshal(answer)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusNoContent {
			resp.Body.Close()
			return nil
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return fmt.Errorf("fileedit: browser exchange returned %s", resp.Status)
		}
		var command BrowseCommand
		err = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&command)
		resp.Body.Close()
		if err != nil {
			return err
		}
		if command.Op != OpList && command.Op != OpRead && command.Op != OpSearch && command.Op != OpExtensions {
			return fmt.Errorf("fileedit: browser refused operation %q", command.Op)
		}
		res, err := Execute(worldRoot, Request{Op: command.Op, Path: command.Path, Query: command.Query})
		answer = BrowseAnswer{ID: command.ID}
		if err != nil {
			answer.Error = err.Error()
			continue
		}
		answer.Result, err = json.Marshal(res)
		if err != nil {
			return err
		}
	}
}

const (
	BrowserTokenEnv = "FELIS_FILE_BROWSER_TOKEN"
	BrowserRoute    = "/api/v1/internal/file-browser/"
	browserIdle     = 45 * time.Second
	browserLifetime = 4 * time.Minute
	maxBrowsers     = 4
)

var errBrowserFull = errors.New("fileedit: browser capacity reached")

// Browser reuses a read-only Job per world over the internal API without additional inbound ports or RBAC.
type Browser struct {
	BaseURL  string
	mu       sync.Mutex
	sessions map[string]*browseSession
}

type BrowseCommand struct {
	ID    string `json:"id"`
	Op    string `json:"op"`
	Path  string `json:"path"`
	Query string `json:"query,omitempty"`
}

type BrowseAnswer struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

type browseSession struct {
	id, key   string
	tokenHash [sha256.Size]byte
	commands  chan BrowseCommand
	answers   chan BrowseAnswer
	serial    chan struct{}
	done      chan struct{}
	once      sync.Once
	timer     *time.Timer
	mu        sync.Mutex
	connected bool
	pending   string
}

func (b *Browser) close(s *browseSession) {
	b.mu.Lock()
	if b.sessions[s.key] == s {
		delete(b.sessions, s.key)
	}
	b.mu.Unlock()
	s.once.Do(func() {
		if s.timer != nil {
			s.timer.Stop()
		}
		close(s.done)
	})
}

func (b *Browser) session(ctx context.Context, p JobParams, start func(context.Context, JobParams) error) (*browseSession, error) {
	key := strings.Join([]string{p.Namespace, p.Server, p.WorldPVC, p.NodeName, p.Image}, "\x00")
	b.mu.Lock()
	defer b.mu.Unlock()
	if s := b.sessions[key]; s != nil {
		return s, nil
	}
	if len(b.sessions) >= maxBrowsers {
		return nil, errBrowserFull
	}
	token, err := randomHex(32)
	if err != nil {
		return nil, err
	}
	s := &browseSession{id: p.OpID, key: key, tokenHash: sha256.Sum256([]byte(token)),
		commands: make(chan BrowseCommand, 1), answers: make(chan BrowseAnswer, 1), serial: make(chan struct{}, 1), done: make(chan struct{})}
	if b.sessions == nil {
		b.sessions = make(map[string]*browseSession)
	}
	b.sessions[key] = s
	p.BrowserURL, p.BrowserToken = strings.TrimRight(b.BaseURL, "/")+BrowserRoute+s.id, token
	p.Deadline = browserLifetime
	if err := start(ctx, p); err != nil {
		delete(b.sessions, key)
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		delete(b.sessions, key)
		return nil, err
	}
	s.timer = time.AfterFunc(browserLifetime, func() { b.close(s) })
	return s, nil
}

func (b *Browser) Run(ctx context.Context, p JobParams, start func(context.Context, JobParams) error) ([]byte, error) {
	s, err := b.session(ctx, p, start)
	if err != nil {
		return nil, err
	}
	select {
	case s.serial <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
		return nil, fmt.Errorf("fileedit: browser closed")
	}
	defer func() { <-s.serial }()
	s.mu.Lock()
	s.pending = p.OpID
	s.mu.Unlock()
	select {
	case s.commands <- BrowseCommand{ID: p.OpID, Op: p.Op, Path: p.Path, Query: p.Query}:
	case <-ctx.Done():
		b.close(s)
		return nil, ctx.Err()
	case <-s.done:
		return nil, fmt.Errorf("fileedit: browser closed")
	}
	select {
	case answer := <-s.answers:
		if answer.Error != "" {
			return nil, fmt.Errorf("fileedit: browser read: %s", answer.Error)
		}
		return answer.Result, nil
	case <-ctx.Done():
		b.close(s)
		return nil, ctx.Err()
	case <-s.done:
		return nil, fmt.Errorf("fileedit: browser closed before returning a result")
	}
}

// ServeHTTP authenticates each worker with a world-scoped token and long-polls its next command.
func (b *Browser) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, BrowserRoute)
	b.mu.Lock()
	var s *browseSession
	for _, candidate := range b.sessions {
		if candidate.id == id {
			s = candidate
			break
		}
	}
	b.mu.Unlock()
	token, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	sum := sha256.Sum256([]byte(token))
	if s == nil || !bearer || subtle.ConstantTimeCompare(sum[:], s.tokenHash[:]) != 1 {
		http.Error(w, "unknown browser", http.StatusNotFound)
		return
	}
	var answer BrowseAnswer
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxLogBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&answer); err != nil {
		http.Error(w, "invalid result", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	valid := !s.connected && answer.ID == "" || s.connected && answer.ID != "" && answer.ID == s.pending
	if valid {
		s.connected = true
		if answer.ID != "" {
			s.pending = ""
		}
	}
	s.mu.Unlock()
	if !valid {
		http.Error(w, "unexpected result", http.StatusConflict)
		return
	}
	if answer.ID != "" {
		select {
		case s.answers <- answer:
		case <-s.done:
			w.WriteHeader(http.StatusGone)
			return
		}
	}
	timer := time.NewTimer(browserIdle)
	defer timer.Stop()
	select {
	case command := <-s.commands:
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(command); err != nil {
			b.close(s)
		}
	case <-timer.C:
		b.close(s)
		w.WriteHeader(http.StatusNoContent)
	case <-s.done:
		w.WriteHeader(http.StatusNoContent)
	case <-r.Context().Done():
		b.close(s)
	}
}
