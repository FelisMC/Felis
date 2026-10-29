package rcon_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"felis.lolicon.best/internal/rcon"
)

func listOn(reply *string) func(*rcon.Conn) error {
	return func(c *rcon.Conn) error {
		out, err := c.Execute("list")
		*reply = out
		return err
	}
}

func TestPoolKeepsOneSessionAcrossCommands(t *testing.T) {
	f := startFakeRCON(t, "pw", map[string]string{"list": "There are 0 of a max of 20 players online"})
	defer f.stop()
	p := rcon.NewPool(rcon.Dial, time.Minute)
	for i := 0; i < 3; i++ {
		var reply string
		if err := p.Do(context.Background(), f.addr(), "pw", time.Second, listOn(&reply)); err != nil || reply != "There are 0 of a max of 20 players online" {
			t.Fatalf("command %d: reply %q, err %v", i, reply, err)
		}
	}
	if n := f.sessions(); n != 1 {
		t.Fatalf("three commands opened %d sessions, want 1", n)
	}
}

func TestPoolOpensAFreshSessionWhenTheServerClosedTheKeptOne(t *testing.T) {
	f := startFakeRCON(t, "pw", map[string]string{"list": "ok"})
	defer f.stop()
	p := rcon.NewPool(rcon.Dial, time.Minute)
	var reply string
	if err := p.Do(context.Background(), f.addr(), "pw", time.Second, listOn(&reply)); err != nil {
		t.Fatal(err)
	}
	f.dropAll()
	time.Sleep(20 * time.Millisecond)
	if err := p.Do(context.Background(), f.addr(), "pw", time.Second, listOn(&reply)); err != nil || reply != "ok" {
		t.Fatalf("after the server closed the session: reply %q, err %v", reply, err)
	}
	if n := f.sessions(); n != 2 {
		t.Fatalf("sessions = %d, want 2", n)
	}
}

func TestPoolDropsTheSessionAfterAFailureAndWhenThePasswordChanges(t *testing.T) {
	f := startFakeRCON(t, "pw", map[string]string{"list": "ok"})
	defer f.stop()
	p := rcon.NewPool(rcon.Dial, time.Minute)
	failed := errors.New("lost")
	if err := p.Do(context.Background(), f.addr(), "pw", time.Second, func(*rcon.Conn) error { return failed }); !errors.Is(err, failed) {
		t.Fatalf("err = %v, want the command's failure", err)
	}
	var reply string
	if err := p.Do(context.Background(), f.addr(), "pw", time.Second, listOn(&reply)); err != nil {
		t.Fatal(err)
	}
	if err := p.Do(context.Background(), f.addr(), "other", time.Second, listOn(&reply)); !errors.Is(err, rcon.ErrDial) {
		t.Fatalf("a changed password reused the old session: err = %v", err)
	}
	if n := f.sessions(); n != 3 {
		t.Fatalf("sessions = %d, want 3", n)
	}
}

func TestPoolDoAgainRepeatsOnlyWhatFailedOnAKeptSession(t *testing.T) {
	f := startFakeRCON(t, "pw", map[string]string{"list": "ok"})
	defer f.stop()
	p := rcon.NewPool(rcon.Dial, time.Minute)
	calls := 0
	failFirst := func(c *rcon.Conn) error {
		calls++
		if calls == 1 {
			return errors.New("lost")
		}
		_, err := c.Execute("list")
		return err
	}
	if err := p.DoAgain(context.Background(), f.addr(), "pw", time.Second, failFirst); err == nil || calls != 1 {
		t.Fatalf("a failure on a new session was repeated: err %v, calls %d", err, calls)
	}
	var reply string
	if err := p.Do(context.Background(), f.addr(), "pw", time.Second, listOn(&reply)); err != nil {
		t.Fatal(err)
	}
	calls = 0
	if err := p.DoAgain(context.Background(), f.addr(), "pw", time.Second, failFirst); err != nil || calls != 2 {
		t.Fatalf("a failure on a kept session: err %v, calls %d; want nil and 2", err, calls)
	}
}

func TestPoolClosesSessionsLeftIdle(t *testing.T) {
	a := startFakeRCON(t, "pw", map[string]string{"list": "a"})
	defer a.stop()
	b := startFakeRCON(t, "pw", map[string]string{"list": "b"})
	defer b.stop()
	p := rcon.NewPool(rcon.Dial, 0)
	var reply string
	for _, addr := range []string{a.addr(), b.addr(), a.addr()} {
		if err := p.Do(context.Background(), addr, "pw", time.Second, listOn(&reply)); err != nil {
			t.Fatal(err)
		}
	}
	if n := a.sessions(); n != 2 {
		t.Fatalf("an idle session was kept: sessions on a = %d, want 2", n)
	}
}

func TestPoolHandsOutASessionWithoutTheLastCallersDeadline(t *testing.T) {
	f := startFakeRCON(t, "pw", map[string]string{"list": "ok"})
	defer f.stop()
	p := rcon.NewPool(rcon.Dial, time.Minute)
	short := func(c *rcon.Conn) error {
		if err := c.SetDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
			return err
		}
		_, err := c.Execute("list")
		return err
	}
	if err := p.Do(context.Background(), f.addr(), "pw", time.Second, short); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	var reply string
	if err := p.Do(context.Background(), f.addr(), "pw", time.Second, listOn(&reply)); err != nil || reply != "ok" {
		t.Fatalf("the next caller inherited an expired deadline: reply %q, err %v", reply, err)
	}
	if n := f.sessions(); n != 1 {
		t.Fatalf("sessions = %d, want 1", n)
	}
}

func TestPoolWaitsForTheSessionOrTheCallersContext(t *testing.T) {
	f := startFakeRCON(t, "pw", map[string]string{"list": "ok"})
	defer f.stop()
	p := rcon.NewPool(rcon.Dial, time.Minute)
	held, release := make(chan struct{}), make(chan struct{})
	go func() {
		_ = p.Do(context.Background(), f.addr(), "pw", time.Second, func(*rcon.Conn) error { close(held); <-release; return nil })
	}()
	<-held
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := p.Do(ctx, f.addr(), "pw", time.Second, func(*rcon.Conn) error { t.Error("ran while another caller held the session"); return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the caller's deadline", err)
	}
	close(release)
}
