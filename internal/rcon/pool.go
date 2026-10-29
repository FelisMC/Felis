package rcon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Session is a connection a Pool may keep between uses.
type Session interface {
	Ready() bool
	Close() error
}

// ErrDial marks a session that could not be opened, so no command was sent.
var ErrDial = errors.New("rcon: cannot open a session")

// Pool keeps one session per address because servers log every RCON session they open; a session serves one caller at a time.
type Pool[S Session] struct {
	dial  func(addr, password string, timeout time.Duration) (S, error)
	idle  time.Duration
	mu    sync.Mutex
	slots map[string]*poolSlot[S]
}

type poolSlot[S Session] struct {
	token    chan struct{}
	refs     int
	live     bool
	session  S
	password string
	used     time.Time
}

// NewPool closes sessions nobody has used for idle.
func NewPool[S Session](dial func(addr, password string, timeout time.Duration) (S, error), idle time.Duration) *Pool[S] {
	return &Pool[S]{dial: dial, idle: idle, slots: map[string]*poolSlot[S]{}}
}

// Do runs fn on the address's session, opening one when none is kept or it went away; a failing fn drops the session.
func (p *Pool[S]) Do(ctx context.Context, addr, password string, timeout time.Duration, fn func(S) error) error {
	_, err := p.do(ctx, addr, password, timeout, fn)
	return err
}

// DoAgain is Do for commands that are safe to repeat: when a kept session fails, fn runs once more on a new one.
func (p *Pool[S]) DoAgain(ctx context.Context, addr, password string, timeout time.Duration, fn func(S) error) error {
	kept, err := p.do(ctx, addr, password, timeout, fn)
	if err == nil || !kept || errors.Is(err, ErrDial) || ctx.Err() != nil {
		return err
	}
	_, err = p.do(ctx, addr, password, timeout, fn)
	return err
}

func (p *Pool[S]) do(ctx context.Context, addr, password string, timeout time.Duration, fn func(S) error) (kept bool, err error) {
	s, err := p.acquire(ctx, addr)
	if err != nil {
		return false, err
	}
	defer p.release(addr, s)
	if s.live && (s.password != password || !s.session.Ready()) {
		_ = s.session.Close()
		s.live = false
	}
	kept = s.live
	if !s.live {
		session, err := p.dial(addr, password, timeout)
		if err != nil {
			return false, fmt.Errorf("%w: %w", ErrDial, err)
		}
		s.session, s.password, s.live = session, password, true
	}
	if err := fn(s.session); err != nil {
		_ = s.session.Close()
		s.live = false
		return kept, err
	}
	return kept, nil
}

func (p *Pool[S]) acquire(ctx context.Context, addr string) (*poolSlot[S], error) {
	p.mu.Lock()
	s := p.slots[addr]
	if s == nil {
		s = &poolSlot[S]{token: make(chan struct{}, 1)}
		s.token <- struct{}{}
		p.slots[addr] = s
	}
	s.refs++
	p.mu.Unlock()
	select {
	case <-s.token:
		return s, nil
	case <-ctx.Done():
		p.mu.Lock()
		s.refs--
		p.mu.Unlock()
		return nil, ctx.Err()
	}
}

// release also closes other sessions left idle; a slot nobody holds or waits for is safe to touch under mu.
func (p *Pool[S]) release(addr string, s *poolSlot[S]) {
	now := time.Now()
	s.used = now
	p.mu.Lock()
	s.refs--
	for a, other := range p.slots {
		if other.refs > 0 {
			continue
		}
		if other.live && a != addr && now.Sub(other.used) >= p.idle {
			_ = other.session.Close()
			other.live = false
		}
		if !other.live {
			delete(p.slots, a)
		}
	}
	p.mu.Unlock()
	s.token <- struct{}{}
}
