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
			want:   PlayerCount{Online: 0, Max: 20},
			wantOK: true,
		},
		{
			name:   "with player names",
			reply:  "There are 3 of a max of 20 players online: alice, bob, carol",
			want:   PlayerCount{Online: 3, Max: 20},
			wantOK: true,
		},
		{
			name:   "full server",
			reply:  "There are 20 of a max of 20 players online: ...",
			want:   PlayerCount{Online: 20, Max: 20},
			wantOK: true,
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
