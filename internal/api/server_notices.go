package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/metrics"
)

const (
	// Events older than serverNoticeMaxAge are marked handled without mail.
	serverNoticeMaxAge = time.Hour
	// crashMailHold lets a crash settle so the restart outcome lands in the same mail.
	crashMailHold = 3 * time.Minute
)

var errMailBudget = errors.New("mail budget spent")

func (a *API) NotifyServerEvents(ctx context.Context) error {
	feeds, err := a.Cluster.ServerEventFeeds(ctx)
	if err != nil {
		return fmt.Errorf("list server events: %w", err)
	}
	now := a.now()
	for i := range feeds {
		if err := a.mailServerEvents(ctx, &feeds[i], now); err != nil {
			log.Printf("api: lifecycle mail for %s: %v", feeds[i].Name, err)
		}
	}
	return nil
}

func (a *API) mailServerEvents(ctx context.Context, f *ServerEventFeed, now time.Time) error {
	var pending []ServerEvent
	for _, ev := range f.Events {
		if ev.Seq > f.Mailed {
			pending = append(pending, ev)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	last := pending[len(pending)-1]
	if last.Type == v1alpha1.EventCrashed && !last.Stopping && now.Sub(pending[0].At) < crashMailHold {
		return nil
	}
	var mail []ServerEvent
	for i, ev := range pending {
		if now.Sub(ev.At) <= serverNoticeMaxAge && mailWorthy(f.Notify, pending, i) {
			mail = append(mail, ev)
		}
	}
	if len(mail) > 0 && !f.System {
		if err := a.sendServerNotice(ctx, f, mail); err != nil {
			return err
		}
	}
	return a.Cluster.MarkEventsMailed(ctx, f.Name, last.Seq)
}

func isProblem(ev ServerEvent) bool {
	switch ev.Type {
	case v1alpha1.EventCrashed, v1alpha1.EventHostRestarted, v1alpha1.EventStartFailed, v1alpha1.EventDiskWarning, v1alpha1.EventDiskFull:
		return true
	}
	return false
}

func mailWorthy(n ServerNotify, batch []ServerEvent, i int) bool {
	ev := batch[i]
	switch ev.Type {
	case v1alpha1.EventStarted:
		return n.Started || (n.Problems && i > 0 && isProblem(batch[i-1]))
	case v1alpha1.EventStopped:
		// A stop caused by a problem is already in that problem's mail.
		return n.Stopped && !(n.Problems && ev.Reason != "" && ev.Reason != v1alpha1.StopIdle)
	}
	return n.Problems && isProblem(ev)
}

func (a *API) sendServerNotice(ctx context.Context, f *ServerEventFeed, events []ServerEvent) error {
	to, err := a.Repo.ServerNoticeRecipients(ctx, f.Name)
	if err != nil {
		return fmt.Errorf("resolve recipients: %w", err)
	}
	if len(to) == 0 {
		return nil
	}
	sender, ok := a.mailer(ctx).(noticeSender)
	if !ok {
		return nil
	}
	subject, body := renderServerNotice(f, events, a.panelURL())
	for _, addr := range to {
		if ok, _ := a.mailGate().take(mailGateKey); !ok {
			metrics.MailTotal.WithLabelValues("notice", "throttled").Inc()
			return errMailBudget
		}
		if err := sender.SendNotice(ctx, addr, subject, body); err != nil {
			metrics.MailTotal.WithLabelValues("notice", "failed").Inc()
			return err
		}
		metrics.MailTotal.WithLabelValues("notice", "sent").Inc()
	}
	return nil
}

// eventRank orders events by how much a subject line should lead with them.
func eventRank(ev ServerEvent) int {
	switch {
	case ev.Stopping:
		return 5
	case ev.Type == v1alpha1.EventDiskFull:
		return 5
	case ev.Type == v1alpha1.EventStartFailed:
		return 4
	case ev.Type == v1alpha1.EventCrashed:
		return 3
	case ev.Type == v1alpha1.EventHostRestarted:
		return 2
	case ev.Type == v1alpha1.EventDiskWarning:
		return 2
	case ev.Type == v1alpha1.EventStopped:
		return 1
	}
	return 0
}

func renderServerNotice(f *ServerEventFeed, events []ServerEvent, panel string) (subject, body string) {
	lead := events[0]
	for _, ev := range events[1:] {
		if eventRank(ev) >= eventRank(lead) {
			lead = ev
		}
	}
	zhHead, enHead := eventHeadline(lead)
	label := f.Name
	if f.DisplayName != "" && f.DisplayName != f.Name {
		label = f.DisplayName + " (" + f.Name + ")"
	}
	subject = "Felis 服务器 " + label + " " + zhHead + " · Server " + label + " " + enHead

	var zh, en strings.Builder
	fmt.Fprintf(&zh, "服务器 %s：\n", label)
	fmt.Fprintf(&en, "Server %s:\n", label)
	for _, ev := range events {
		when := ev.At.UTC().Format("2006-01-02 15:04 MST")
		zhLine, enLine := eventLine(ev)
		fmt.Fprintf(&zh, "· %s %s\n", when, zhLine)
		fmt.Fprintf(&en, "· %s %s\n", when, enLine)
	}
	if panel != "" {
		fmt.Fprintf(&zh, "在控制台查看：%s/servers/%s\n", panel, f.Name)
		fmt.Fprintf(&en, "Open it in the console: %s/servers/%s\n", panel, f.Name)
	}
	zh.WriteString("通知设置位于服务器的“编辑”对话框。\n")
	en.WriteString("Notification settings are in the server's Edit dialog.\n")
	return subject, zh.String() + "\n" + en.String()
}

func eventHeadline(ev ServerEvent) (zh, en string) {
	switch ev.Type {
	case v1alpha1.EventDiskWarning:
		return "磁盘配额接近上限", "is nearing its disk quota"
	case v1alpha1.EventDiskFull:
		return "磁盘配额已满", "reached its disk quota"
	case v1alpha1.EventStarted:
		return "已启动", "started"
	case v1alpha1.EventStopped:
		return "已停止", "stopped"
	case v1alpha1.EventStartFailed:
		return "启动失败", "failed to start"
	case v1alpha1.EventHostRestarted:
		if ev.Stopping {
			return "在主机重启后保持停止", "stays stopped after a host restart"
		}
		return "在主机重启后恢复运行", "resumed after a host restart"
	}
	if ev.Stopping {
		return "崩溃后已停止", "stopped after crashing"
	}
	return "进程意外退出", "crashed"
}

func eventLine(ev ServerEvent) (zh, en string) {
	switch ev.Type {
	case v1alpha1.EventDiskWarning:
		return "世界卷用量已达到磁盘配额的 90%，请及时清理文件。", "The world volume has used at least 90% of its disk quota; remove unneeded files."
	case v1alpha1.EventDiskFull:
		if ev.Stopping {
			return "世界卷已达到磁盘配额，正在正常停服。清理文件并等待成功采样后可重新启动。", "The world volume reached its disk quota and is stopping normally. Remove files and wait for a successful usage sample before starting again."
		}
		return "世界卷已达到磁盘配额，暂不能启动。清理文件并等待成功采样后可重新启动。", "The world volume reached its disk quota and cannot start. Remove files and wait for a successful usage sample before starting again."
	case v1alpha1.EventStarted:
		return "服务器已启动。", "The server started."
	case v1alpha1.EventStopped:
		switch ev.Reason {
		case v1alpha1.StopIdle:
			return "服务器无人在线，已自动停止。", "The server sat empty and stopped on its own."
		case v1alpha1.StopCrashed, v1alpha1.StopCrashLimit:
			return "服务器在崩溃后停止。", "The server stopped after crashing."
		case v1alpha1.StopHostRestart:
			return "服务器在主机重启后保持停止。", "The server stayed stopped after a host restart."
		case v1alpha1.StopDiskFull:
			return "服务器已因磁盘配额满而停止。", "The server stopped because its disk quota was full."
		}
		return "服务器已停止。", "The server stopped."
	case v1alpha1.EventStartFailed:
		zh, en = "服务器启动失败，自动重试次数已用完", "The server failed to start and its automatic retries are spent"
		switch ev.Reason {
		case v1alpha1.ReasonStartupTimeout:
			zh, en = zh+"（启动超时）", en+" (the start timed out)"
		case v1alpha1.ReasonReadinessTimeout:
			zh, en = zh+"（RCON 未响应）", en+" (RCON never answered)"
		}
		return zh + "。请查看日志后在控制台重新启动。", en + ". Check the log, then start it again from the console."
	case v1alpha1.EventHostRestarted:
		if ev.Stopping {
			return "主机曾重启（例如断电）。该服务器设置为主机重启后不恢复运行，现保持停止。",
				"The host restarted, for example after a power loss. The server is set not to resume and stays stopped."
		}
		return "主机曾重启（例如断电），服务器正在恢复运行。",
			"The host restarted, for example after a power loss, and the server is resuming."
	}
	zh, en = crashCause(ev)
	switch {
	case !ev.Stopping && ev.Limit > 0:
		return fmt.Sprintf("%s，已自动重启（连续第 %d 次，上限 %d 次）。", zh, ev.Crashes, ev.Limit),
			fmt.Sprintf("%s and was restarted (crash %d in a row, limit %d).", en, ev.Crashes, ev.Limit)
	case !ev.Stopping:
		return zh + "，已自动重启。", en + " and was restarted."
	case ev.Limit == 0:
		return zh + "。该服务器设置为崩溃后不自动重启，现已停止。",
			en + ". The server is set not to restart after a crash and has stopped."
	}
	return fmt.Sprintf("%s。已连续崩溃 %d 次，超过自动重启上限 %d 次，现已停止。修复后请在控制台启动。", zh, ev.Crashes, ev.Limit),
		fmt.Sprintf("%s. That is crash %d in a row, over the limit of %d restarts, so the server has stopped. Start it from the console once it is fixed.", en, ev.Crashes, ev.Limit)
}

func crashCause(ev ServerEvent) (zh, en string) {
	switch ev.Reason {
	case "OOMKilled":
		return "服务器内存不足，进程被终止", "The server ran out of memory and its process was killed"
	case "Completed":
		return "服务器进程自行退出（退出码 0）", "The server process exited on its own (exit code 0)"
	}
	return fmt.Sprintf("服务器进程意外退出（退出码 %d）", ev.ExitCode),
		fmt.Sprintf("The server process exited unexpectedly (exit code %d)", ev.ExitCode)
}
