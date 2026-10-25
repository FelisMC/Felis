package api

import (
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
)

func TestDiskQuotaEventsUseProblemMailAndDoNotRepeat(t *testing.T) {
	for _, event := range []string{v1alpha1.EventDiskWarning, v1alpha1.EventDiskFull} {
		t.Run(event, func(t *testing.T) {
			for _, problems := range []bool{false, true} {
				a, cl, mailer, now := noticeAPI(t, ServerNotify{Problems: problems}, ServerEvent{Seq: 1, Type: event, At: time.Date(2026, 10, 21, 7, 59, 0, 0, time.UTC), Stopping: event == v1alpha1.EventDiskFull})
				tick(t, a)
				want := 0
				if problems {
					want = 1
				}
				if len(mailer.notices) != want || cl.mailed["survival"] != 1 {
					t.Fatal("disk events ignored the problem preference or were not acknowledged")
				}
				if problems {
					if !strings.Contains(mailer.notices[0], "磁盘配额") || !strings.Contains(mailer.notices[0], "disk quota") || !strings.Contains(mailer.notices[0], "/servers/survival") {
						t.Fatal("disk notice omitted its cause or console link")
					}
					if strings.Contains(mailer.notices[0], "crashed") {
						t.Fatal("a disk quota event was described as a crash")
					}
				}
				cl.feeds[0].Events = append(cl.feeds[0].Events, ServerEvent{Seq: 2, Type: v1alpha1.EventStopped, Reason: v1alpha1.StopDiskFull, At: *now})
				tick(t, a)
				tick(t, a)
				if len(mailer.notices) != want || cl.mailed["survival"] != 2 {
					t.Fatal("disk stop or a repeated mail pass duplicated the problem notice")
				}
			}
		})
	}
}
