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

	"felis.lolicon.best/internal/offsite"
)

// defaultHeartbeatFile holds the heartbeat URL deploy/bootstrap.sh writes from
// FELIS_WATCHDOG_HEARTBEAT_URL. It lives in /etc/felis, so a host rebuilt from
// a bundle pings the same check once it takes the off-site bucket over.
const defaultHeartbeatFile = "/etc/felis/watchdog-heartbeat-url"

const (
	// heartbeatTimeout bounds one ping; a monitoring service slower than that
	// is as good as down for the run.
	heartbeatTimeout = 10 * time.Second
	// heartbeatReportMax caps the report a failure ping carries: monitoring
	// services keep about the first 10 KB of a ping's body.
	heartbeatReportMax = 10000
)

// heartbeat is the ping one run sends to a dead man's switch at a monitoring
// service (Healthchecks.io, and the services that copy its API), which alerts
// its own users when the pings stop: the host down, the timer gone, the
// watchdog failing before it can mail. No check on the host can report those.
// A run whose alerts reach the owners GETs url; one whose alerts reach no one
// POSTs report to url/fail, or withholds the ping when url has a query, where
// no /fail can be added and the missing ping trips the check instead.
type heartbeat struct {
	url    string // "" pings nothing
	fail   bool
	report string
	// standby is a host standing by for another host's off-site bucket: a
	// rehearsal, or a rebuild not taken over. It pings nothing, or it would
	// keep the check of the host that writes the bucket green after that host
	// died.
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
	if err := checkHeartbeatURL(s); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// checkHeartbeatURL accepts an http(s) URL with a host. Its error leaves the
// URL out: the path is the check's key, which anyone who reads it can ping in
// the host's name.
func checkHeartbeatURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || strings.ContainsAny(s, " \t\r\n") {
		return errors.New("the heartbeat URL is not an http:// or https:// URL")
	}
	return nil
}

// redactURL is a heartbeat URL as logs show it: its scheme and host.
func redactURL(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "(the heartbeat URL)"
	}
	return u.Scheme + "://" + u.Host + "/..."
}

// standsBy reports whether this host stands by for another host's off-site
// bucket (offsite.Status.Standby), however old that record is: the
// installer's take-over or the host's first write ends it.
func standsBy(offsiteOn bool, statusPath string) bool {
	if !offsiteOn {
		return false
	}
	st, err := offsite.ReadStatus(statusPath)
	return err == nil && st != nil && st.Standby
}
