package operator

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

func TestParseListReply(t *testing.T) {
	cases := []struct {
		name   string
		reply  string
		want   PlayerCount
		wantOK bool
	}{
		{
			name:   "empty server",
			reply:  "There are 0 of a max of 20 players online:",
			want:   PlayerCount{Online: 0, Max: 20, Known: true},
			wantOK: true,
		},
		{
			name:   "with player names",
			reply:  "There are 3 of a max of 20 players online: alice, bob, carol",
			want:   PlayerCount{Online: 3, Max: 20, Known: true},
			wantOK: true,
		},
		{
			name:   "full server",
			reply:  "There are 20 of a max of 20 players online: ...",
			want:   PlayerCount{Online: 20, Max: 20, Known: true},
			wantOK: true,
		},
		{
			name:   "color codes around the numbers",
			reply:  "§6There are §c2§6 of a max of §c50§6 players online:§r alice, bob",
			want:   PlayerCount{Online: 2, Max: 50, Known: true},
			wantOK: true,
		},
		{
			name:   "vanilla 1.12 and Bukkit slash form",
			reply:  "There are 4/32 players online:\nalice, bob, carol, dave",
			want:   PlayerCount{Online: 4, Max: 32, Known: true},
			wantOK: true,
		},
		{
			name:   "EssentialsX",
			reply:  "§6There are §c0§6 out of maximum §c20§6 players online.",
			want:   PlayerCount{Online: 0, Max: 20, Known: true},
			wantOK: true,
		},
		{
			name:   "EssentialsX with vanished players counts them online",
			reply:  "§6There are §c1§6/§c2§6 out of maximum §c20§6 players online.",
			want:   PlayerCount{Online: 3, Max: 20, Known: true},
			wantOK: true,
		},
		{
			name:   "translated reply yields no sample",
			reply:  "当前有 0 个玩家在线，最大在线人数为 20 个玩家。",
			want:   PlayerCount{},
			wantOK: false,
		},
		{
			name:   "unrecognized reply yields no sample",
			reply:  "Unknown command. Try /help for a list of commands.",
			want:   PlayerCount{},
			wantOK: false,
		},
		{
			name:   "empty reply yields no sample",
			reply:  "",
			want:   PlayerCount{},
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseListReply(tc.reply)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if got != tc.want {
				t.Errorf("count = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// serveFakeRcon accepts one RCON connection, accepts any password, and answers
// each command after delay. Every command body is sent on the returned channel.
func serveFakeRcon(t *testing.T, delay time.Duration) (string, <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	cmds := make(chan string, 4)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var hdr [12]byte
			if _, err := io.ReadFull(conn, hdr[:]); err != nil {
				return
			}
			size := int32(binary.LittleEndian.Uint32(hdr[0:]))
			id := int32(binary.LittleEndian.Uint32(hdr[4:]))
			typ := int32(binary.LittleEndian.Uint32(hdr[8:]))
			rest := make([]byte, size-8)
			if _, err := io.ReadFull(conn, rest); err != nil {
				return
			}
			reply := ""
			if typ == 3 { // auth: answer with an auth response carrying the same id
				typ = 2
			} else {
				cmds <- string(rest[:len(rest)-2])
				time.Sleep(delay)
				typ, reply = 0, "Saved the game"
			}
			out := binary.LittleEndian.AppendUint32(nil, uint32(4+4+len(reply)+2))
			out = binary.LittleEndian.AppendUint32(out, uint32(id))
			out = binary.LittleEndian.AppendUint32(out, uint32(typ))
			out = append(append(out, reply...), 0, 0)
			if _, err := conn.Write(out); err != nil {
				return
			}
		}
	}()
	return ln.Addr().String(), cmds
}

func TestRconProberSaveFlushes(t *testing.T) {
	addr, cmds := serveFakeRcon(t, 0)
	if err := (RconProber{}).Save(context.Background(), addr, "pw"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got := <-cmds; got != "save-all flush" {
		t.Errorf("command = %q, want save-all flush", got)
	}
}

func TestRconProberSaveTimesOut(t *testing.T) {
	addr, _ := serveFakeRcon(t, time.Second)
	start := time.Now()
	err := (RconProber{SaveTimeout: 100 * time.Millisecond}).Save(context.Background(), addr, "pw")
	if err == nil {
		t.Fatal("Save returned nil for a reply slower than SaveTimeout")
	}
	if elapsed := time.Since(start); elapsed > 900*time.Millisecond {
		t.Errorf("Save took %v, want it bounded by SaveTimeout", elapsed)
	}
}
