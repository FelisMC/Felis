package operator

import "testing"

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
