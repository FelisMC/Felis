package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"felis.lolicon.best/internal/heartbeatsettings"
	"felis.lolicon.best/internal/offsite"
	"felis.lolicon.best/internal/watchdog"
)

// Keep heartbeat configuration under /etc/felis so recovered hosts retain the monitoring identity.
const defaultHeartbeatFile = "/etc/felis/watchdog-heartbeat-url"

const (
	// heartbeatTimeout bounds one ping; a monitoring service slower than that
	// is as good as down for the run.
	heartbeatTimeout = 10 * time.Second
	// heartbeatReportMax caps the report a failure ping carries: monitoring
	// services keep about the first 10 KB of a ping's body.
	heartbeatReportMax = 10000
)

// External monitoring detects host outages; report failure through /fail or suppress query-bearing pings.
type heartbeat struct {
	url    string // "" pings nothing
	fail   bool
	report string
	// standby is a host standing by for another host's off-site bucket. It pings nothing, or it would keep the check of the host that writes the bucket green after that host died.
	standby bool
	// quiet is the installer's quiet window, which restarts things on
	// purpose: no failure is pinged.
	quiet bool
}

// send pings the heartbeat and logs the outcome.
func (b heartbeat) send(cl *http.Client, stdout, stderr io.Writer) {
	switch {
	case b.url == "":
		return
	case b.standby:
		fmt.Fprintln(stdout, "felis watchdog: this host stands by for the off-site bucket; pinging no heartbeat (the host that writes the bucket pings it)")
		return
	case b.fail && b.quiet:
		fmt.Fprintln(stdout, "felis watchdog: quiet while the installer runs; withholding the failure ping")
		return
	}
	sent, err := b.ping(cl)
	switch {
	case err != nil:
		fmt.Fprintf(stderr, "felis watchdog: heartbeat: %v\n", err)
	case !sent:
		fmt.Fprintf(stdout, "felis watchdog: withholding the heartbeat ping (the URL has a query, so it has no /fail endpoint)\n")
	case b.fail:
		fmt.Fprintf(stdout, "felis watchdog: pinged the heartbeat's failure endpoint at %s\n", redactURL(b.url))
	}
}

// ping sends the request; sent is false when a failure withholds it.
func (b heartbeat) ping(cl *http.Client) (sent bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), heartbeatTimeout)
	defer cancel()
	method, target, body := http.MethodGet, b.url, io.Reader(nil)
	if b.fail {
		if strings.Contains(b.url, "?") {
			return false, nil
		}
		method, target = http.MethodPost, strings.TrimSuffix(b.url, "/")+"/fail"
		body = strings.NewReader(clipUTF8(b.report, heartbeatReportMax))
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return false, fmt.Errorf("%s %s: bad URL", method, redactURL(target))
	}
	resp, err := cl.Do(req)
	if err != nil {
		// A url.Error quotes the whole URL, the check's key among it.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return false, fmt.Errorf("%s %s: %w", method, redactURL(target), err)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return false, fmt.Errorf("%s %s: %s", method, redactURL(target), resp.Status)
	}
	return true, nil
}

// clipUTF8 cuts s to at most n bytes without splitting a character.
func clipUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

// readHeartbeatURL reads the heartbeat URL from path; no file is no heartbeat.
func readHeartbeatURL(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(raw))
	if err := heartbeatsettings.Check(s); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// redactURL is a heartbeat URL as logs show it: its scheme and host.
func redactURL(s string) string { return heartbeatsettings.Redact(s) }

// Cache panel heartbeat overrides across database outages.
func heartbeatFor(state *watchdog.State, panel *heartbeatsettings.Settings, panelErr error, file string) string {
	switch {
	case panel != nil:
		state.Heartbeat = panel.URL
	case panelErr == nil:
		state.Heartbeat = ""
	}
	if state.Heartbeat != "" {
		return state.Heartbeat
	}
	return file
}

// Standby remains effective until takeover or this host's first write, regardless of status age.
func standsBy(offsiteOn bool, statusPath string) bool {
	if !offsiteOn {
		return false
	}
	st, err := offsite.ReadStatus(statusPath)
	return err == nil && st != nil && st.Standby
}
