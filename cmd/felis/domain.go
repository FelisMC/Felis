package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/naming"
	"felis.lolicon.best/internal/platform"
	"felis.lolicon.best/internal/store"

	"github.com/BurntSushi/toml"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// felis domain moves an installed platform to a new root domain (#12). The domain
// is rendered into places nothing re-renders afterwards, and a surface left behind
// breaks one feature rather than the whole install:
//
//   - the toml configs (felis.host.toml, felis.pod.toml, and felis.toml when it is
//     a file of its own rather than the link to the host copy);
//   - the panel certificate, which the installer writes once;
//   - the felis-config Secret (and its workload-namespace mirror) and the
//     felis-api-tls Secret, which felis-api mounts;
//   - the proxy's felis-link.properties;
//   - the login gate's MinecraftServer env.
//
// `set` rewrites each of them in place and keeps every other value; `check` reads
// each back, including what the running felis-api, proxy and login pod serve, so a
// half-moved install says which surface is behind. Re-running the installer is not
// the way: it regenerates files an operator has tuned, and it keeps the old panel
// certificate.

const (
	domainUsage = "Usage: felis domain set [-yes] <new-root-domain> | felis domain check"
	// dnsProbeLabel is looked up under the root domain to see whether the wildcard
	// record the game subdomains need exists: no real server is named this.
	dnsProbeLabel = "felis-dns-probe"
	// panelCertDays matches the installer's `openssl req -days 825`.
	panelCertDays = 825
)

var domainLabelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// domainNames are the three names a root domain puts on the install.
type domainNames struct {
	root, panel, admin string
}

func effectiveDomainNames(root, panel, admin string) domainNames {
	return domainNames{root: root, panel: defaultPanelHostname(root, panel), admin: defaultAdminHostname(root, admin)}
}

// domainPlan is what `felis domain set` changes. A hostname that is not the
// default under the old root (console.<root>, op.console.<root>) was set by hand,
// and it stays as it is.
type domainPlan struct {
	from, to                 domainNames
	customPanel, customAdmin bool
}

func planDomainChange(cur domainNames, newRoot string) domainPlan {
	p := domainPlan{from: cur, to: domainNames{root: newRoot, panel: "console." + newRoot, admin: "op.console." + newRoot}}
	if cur.panel != "console."+cur.root {
		p.customPanel, p.to.panel = true, cur.panel
	}
	if cur.admin != "op.console."+cur.root {
		p.customAdmin, p.to.admin = true, cur.admin
	}
	return p
}

// normalizeRootDomain lowercases a root domain and refuses anything that is not
// a DNS name the panel and the game subdomains can live under.
func normalizeRootDomain(s string) (string, error) {
	d := strings.ToLower(strings.Trim(strings.TrimSpace(s), "."))
	switch {
	case d == "":
		return "", errors.New("the new root domain is empty")
	case strings.Contains(d, "://") || strings.ContainsAny(d, "/:@ \t"):
		return "", fmt.Errorf("%q is not a bare domain: give the name alone, without a scheme, port or path", s)
	case net.ParseIP(d) != nil:
		return "", fmt.Errorf("%q is an IP address: the panel and the game subdomains need a DNS name (for a test install, <ip>.nip.io)", s)
	case len("op.console.")+len(d) > 253:
		return "", fmt.Errorf("%q is too long: op.console.<domain> must fit in 253 characters", s)
	}
	labels := strings.Split(d, ".")
	if len(labels) < 2 {
		return "", fmt.Errorf("%q needs at least two labels, e.g. example.com", s)
	}
	for _, l := range labels {
		if !domainLabelRE.MatchString(l) {
			return "", fmt.Errorf("%q is not a valid domain: label %q may hold only a-z, 0-9 and inner hyphens, 63 characters at most", s, l)
		}
	}
	return d, nil
}

// tomlStringEdit sets one string key of one table.
type tomlStringEdit struct{ table, key, value string }

func domainTOMLEdits(n domainNames) []tomlStringEdit {
	return []tomlStringEdit{
		{"server", "root_domain", n.root},
		{"auth", "panel_hostname", n.panel},
		{"auth", "admin_hostname", n.admin},
	}
}

var (
	tomlTableHeaderRE = regexp.MustCompile(`^\s*\[\s*([A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*)\s*\]\s*(#.*)?$`)
	tomlKeyLineRE     = regexp.MustCompile(`^\s*([A-Za-z0-9_-]+)\s*=`)
)

// editTOMLStrings sets string keys in named tables by editing lines, so the
// comments the installer writes to explain the file, every other key and the
// layout stay as they were (a decode/encode round trip drops the comments). A key
// that is missing goes after the last key of its table, and a missing table goes
// at the end. The result is decoded and compared with the original plus the
// intended edits: a file this editor reads differently from the TOML decoder (a
// multi-line value, a quoted or dotted key) is refused rather than half-edited.
func editTOMLStrings(raw []byte, edits []tomlStringEdit) ([]byte, error) {
	var lines []string
	if len(raw) > 0 {
		lines = strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	}
	done := make([]bool, len(edits))
	// last[table] is the line of that table's header or of its last key.
	last := map[string]int{}
	table := ""
	for i, ln := range lines {
		if trimmed := strings.TrimSpace(ln); strings.HasPrefix(trimmed, "[") {
			table = "\x00" // an array table, or a header this editor does not read: never a target
			if m := tomlTableHeaderRE.FindStringSubmatch(ln); m != nil {
				table = m[1]
				last[table] = i
			}
			continue
		}
		m := tomlKeyLineRE.FindStringSubmatch(ln)
		if m == nil {
			continue
		}
		last[table] = i
		for j, e := range edits {
			if e.table == table && e.key == m[1] {
				indent := ln[:len(ln)-len(strings.TrimLeft(ln, " \t"))]
				lines[i] = indent + tomlStringLine(e.key, e.value)
				done[j] = true
			}
		}
	}

	pending := map[string][]string{}
	var newTables []string
	for j, e := range edits {
		if done[j] {
			continue
		}
		if _, ok := last[e.table]; !ok && pending[e.table] == nil {
			newTables = append(newTables, e.table)
		}
		pending[e.table] = append(pending[e.table], tomlStringLine(e.key, e.value))
	}
	var existing []string
	for t := range pending {
		if _, ok := last[t]; ok {
			existing = append(existing, t)
		}
	}
	// Bottom-up, so the positions of the tables above stay valid.
	sort.Slice(existing, func(a, b int) bool { return last[existing[a]] > last[existing[b]] })
	for _, t := range existing {
		at := last[t] + 1
		lines = append(lines[:at], append(append([]string{}, pending[t]...), lines[at:]...)...)
	}
	for _, t := range newTables {
		lines = append(lines, "", "["+t+"]")
		lines = append(lines, pending[t]...)
	}
	out := []byte(strings.Join(lines, "\n") + "\n")
	if err := verifyTOMLEdit(raw, out, edits); err != nil {
		return nil, err
	}
	return out, nil
}

// verifyTOMLEdit proves edited decodes to exactly orig plus the edits.
func verifyTOMLEdit(orig, edited []byte, edits []tomlStringEdit) error {
	want, got := map[string]any{}, map[string]any{}
	if _, err := toml.Decode(string(orig), &want); err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	if _, err := toml.Decode(string(edited), &got); err != nil {
		return fmt.Errorf("the edited file would not parse: %w", err)
	}
	for _, e := range edits {
		t, ok := want[e.table].(map[string]any)
		if !ok {
			if _, taken := want[e.table]; taken {
				return fmt.Errorf("%s is not a table", e.table)
			}
			t = map[string]any{}
			want[e.table] = t
		}
		t[e.key] = e.value
	}
	if !reflect.DeepEqual(want, got) {
		return errors.New("a line edit would change more than the domain keys (a multi-line value, or a quoted or dotted key?)")
	}
	return nil
}

func tomlStringLine(key, value string) string {
	var b strings.Builder
	for _, r := range value {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	return key + ` = "` + b.String() + `"`
}

// tomlDomainNames reads the effective names out of a felis.toml.
func tomlDomainNames(raw []byte) (domainNames, error) {
	var doc struct {
		Server struct {
			RootDomain string `toml:"root_domain"`
		} `toml:"server"`
		Auth struct {
			PanelHostname string `toml:"panel_hostname"`
			AdminHostname string `toml:"admin_hostname"`
		} `toml:"auth"`
	}
	if _, err := toml.Decode(string(raw), &doc); err != nil {
		return domainNames{}, err
	}
	return effectiveDomainNames(doc.Server.RootDomain, doc.Auth.PanelHostname, doc.Auth.AdminHostname), nil
}

// felisIssuedCert reports whether c is a panel certificate Felis made for itself:
// self-signed, and carrying the localhost names the installer always adds. One an
// operator installed (from a CA, or their own) is theirs to replace.
func felisIssuedCert(c *x509.Certificate) bool {
	if !bytes.Equal(c.RawIssuer, c.RawSubject) || c.CheckSignature(c.SignatureAlgorithm, c.RawTBSCertificate, c.Signature) != nil {
		return false
	}
	hasLocalhost := false
	for _, n := range c.DNSNames {
		hasLocalhost = hasLocalhost || n == "localhost"
	}
	hasLoopback := false
	for _, ip := range c.IPAddresses {
		hasLoopback = hasLoopback || ip.Equal(net.IPv4(127, 0, 0, 1))
	}
	return hasLocalhost && hasLoopback
}

func certCovers(c *x509.Certificate, hosts ...string) bool {
	for _, h := range hosts {
		if c.VerifyHostname(h) != nil {
			return false
		}
	}
	return true
}

func readCertFile(path string) (*x509.Certificate, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	for {
		var block *pem.Block
		block, raw = pem.Decode(raw)
		if block == nil {
			return nil, fmt.Errorf("%s holds no certificate", path)
		}
		if block.Type == "CERTIFICATE" {
			return x509.ParseCertificate(block.Bytes)
		}
	}
}

// issuePanelCert makes the certificate the installer would have made for these
// names (ensure_panel_tls_cert): self-signed RSA 2048 for 825 days, naming the
// admin console, the panel and localhost, and the addresses the old one named.
func issuePanelCert(n domainNames, ips []net.IP, now time.Time) (certPEM, keyPEM []byte, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, err
	}
	var dns []string
	for _, h := range []string{n.admin, n.panel, "localhost"} {
		if !containsString(dns, h) {
			dns = append(dns, h)
		}
	}
	addrs := []net.IP{net.IPv4(127, 0, 0, 1)}
	for _, ip := range ips {
		dup := false
		for _, a := range addrs {
			dup = dup || a.Equal(ip)
		}
		if !dup {
			addrs = append(addrs, ip)
		}
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: n.admin},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.AddDate(0, 0, panelCertDays),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dns,
		IPAddresses:           addrs,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}), nil
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// certAction is what `set` does with the panel certificate.
type certAction int

const (
	certKeep    certAction = iota // it already covers the new names
	certReissue                   // Felis made it (or there is none): make a new one
	certForeign                   // the operator's, and it does not cover the new names
)

type domainPaths struct {
	hostTOML, podTOML, defaultTOML string
	cert, key                      string
	linkProps                      string
	tunnelConfig                   string
}

// unitStatus is what systemd reports about the proxy unit.
type unitStatus struct {
	loaded, active bool
	since          time.Time // when it last became active; zero when unknown
}

// liveAPIView is what the running felis-api serves: its /config.json names and
// the certificate it presents.
type liveAPIView struct {
	names domainNames
	cert  *x509.Certificate
}

type domainHost struct {
	paths       domainPaths
	cl          client.Client
	controlNS   string
	rollAPI     func(ctx context.Context) error
	restartUnit func(ctx context.Context, unit string) error
	unitState   func(ctx context.Context, unit string) (unitStatus, error)
	liveAPI     func(ctx context.Context, serverName string) (liveAPIView, error)
	lookupHost  func(ctx context.Context, host string) ([]string, error)
	// passkeys counts the registered passkeys and the users holding them.
	passkeys  func(ctx context.Context) (creds, users int, err error)
	now       func() time.Time
	out       io.Writer
	loginWait time.Duration
	pollEvery time.Duration
}

func cmdDomain(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "set" && args[0] != "check") {
		fmt.Fprintln(stderr, domainUsage)
		fmt.Fprintln(stderr, "Moves the install to a new root domain on every surface that carries it, or checks each of them.")
		return 2
	}
	sub := args[0]
	fs := flag.NewFlagSet("domain "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	yes := false
	if sub == "set" {
		fs.BoolVar(&yes, "yes", false, "apply the change; without it the plan is printed and nothing changes")
	}
	fs.Usage = func() {
		fmt.Fprintln(stderr, domainUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if (sub == "set" && fs.NArg() != 1) || (sub == "check" && fs.NArg() != 0) {
		fs.Usage()
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintf(stderr, "felis domain: refused — it reads the cluster, the panel key and the proxy's config, so it must run as root (try: sudo felis domain %s)\n", strings.Join(args, " "))
		return 1
	}
	cl, err := buildSystemServerClient()
	if err != nil {
		fmt.Fprintf(stderr, "felis domain: %v\n", err)
		return 1
	}
	h := newDomainHost(cl, stdout)
	ctx := context.Background()
	if sub == "check" {
		return h.check(ctx)
	}
	code, err := h.set(ctx, fs.Arg(0), yes)
	if err != nil {
		fmt.Fprintf(stderr, "felis domain set: %v\n", err)
		return 1
	}
	return code
}

func newDomainHost(cl client.Client, out io.Writer) domainHost {
	controlNS := platform.DefaultControlNamespace
	return domainHost{
		paths: domainPaths{
			hostTOML: hostSetupConfigPath, podTOML: podSetupConfigPath, defaultTOML: defaultSetupConfigPath,
			cert: "/etc/felis/panel-tls.crt", key: "/etc/felis/panel-tls.key",
			linkProps: defaultLinkPropsPath, tunnelConfig: defaultTunnelConfigPath,
		},
		cl:        cl,
		controlNS: controlNS,
		rollAPI: func(ctx context.Context) error {
			if err := kubectl(ctx, "-n", controlNS, "rollout", "restart", "deployment/felis-api"); err != nil {
				return err
			}
			return kubectl(ctx, "-n", controlNS, "rollout", "status", "deployment/felis-api", "--timeout=180s")
		},
		restartUnit: func(ctx context.Context, unit string) error { return systemctl(ctx, "restart", unit) },
		unitState:   systemdUnitState,
		liveAPI: func(ctx context.Context, serverName string) (liveAPIView, error) {
			return fetchLiveAPI(ctx, fmt.Sprintf("https://127.0.0.1:%d/config.json", setupPanelNodePort()), serverName)
		},
		lookupHost: net.DefaultResolver.LookupHost,
		passkeys: func(ctx context.Context) (int, int, error) {
			cfg, err := config.Load(hostSetupConfigPath)
			if err != nil {
				return 0, 0, err
			}
			return countPasskeys(ctx, cfg.Database.URL)
		},
		now:       time.Now,
		out:       out,
		loginWait: 3 * time.Minute,
		pollEvery: 3 * time.Second,
	}
}

func countPasskeys(ctx context.Context, url string) (int, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	drv, err := store.Open(ctx, url)
	if err != nil {
		return 0, 0, err
	}
	defer drv.Close()
	var creds, users int
	err = drv.DB().QueryRowContext(ctx, `SELECT count(*), count(DISTINCT user_id) FROM webauthn_credentials`).Scan(&creds, &users)
	return creds, users, err
}

// systemdUnitState reads LoadState, ActiveState and when the unit last became
// active.
func systemdUnitState(ctx context.Context, unit string) (unitStatus, error) {
	out, err := exec.CommandContext(ctx, "systemctl", "show", "--timestamp=unix",
		"-p", "LoadState", "-p", "ActiveState", "-p", "ActiveEnterTimestamp", unit).Output()
	if err != nil {
		return unitStatus{}, fmt.Errorf("systemctl show %s: %w", unit, err)
	}
	return parseUnitShow(string(out)), nil
}

func parseUnitShow(out string) unitStatus {
	var st unitStatus
	for _, ln := range strings.Split(out, "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(ln), "=")
		switch k {
		case "LoadState":
			st.loaded = v == "loaded"
		case "ActiveState":
			st.active = v == "active"
		case "ActiveEnterTimestamp":
			if sec, err := strconv.ParseInt(strings.TrimPrefix(v, "@"), 10, 64); err == nil && sec > 0 {
				st.since = time.Unix(sec, 0)
			}
		}
	}
	return st
}

// fetchLiveAPI reads what felis-api serves on the panel port. The panel
// certificate is self-signed, so verification is skipped: this reads the
// certificate to check its names, it does not trust it.
func fetchLiveAPI(ctx context.Context, url, serverName string) (liveAPIView, error) {
	c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, ServerName: serverName}, // #nosec G402 -- inspected, not trusted
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return liveAPIView{}, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return liveAPIView{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return liveAPIView{}, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	var rc struct {
		RootDomain    string `json:"rootDomain"`
		PanelHostname string `json:"panelHostname"`
		AdminHostname string `json:"adminHostname"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rc); err != nil {
		return liveAPIView{}, fmt.Errorf("GET %s: %w", url, err)
	}
	v := liveAPIView{names: domainNames{root: rc.RootDomain, panel: rc.PanelHostname, admin: rc.AdminHostname}}
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		v.cert = resp.TLS.PeerCertificates[0]
	}
	return v, nil
}

// tomlTarget is a config file carrying the domain: path as configured, and real
// with links resolved, so a rewrite replaces the file and keeps the link.
type tomlTarget struct{ path, real string }

// tomlTargets are the host and pod copies, and felis.toml when it is a file of
// its own rather than the link to the host copy.
func (h domainHost) tomlTargets() ([]tomlTarget, error) {
	var out []tomlTarget
	seen := map[string]bool{}
	for i, p := range []string{h.paths.hostTOML, h.paths.podTOML, h.paths.defaultTOML} {
		real, err := filepath.EvalSymlinks(p)
		if errors.Is(err, fs.ErrNotExist) && i == 2 {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !seen[real] {
			seen[real] = true
			out = append(out, tomlTarget{p, real})
		}
	}
	return out, nil
}

// certPlan decides what happens to the panel certificate for these names.
func (h domainHost) certPlan(to domainNames) (certAction, *x509.Certificate, error) {
	c, err := readCertFile(h.paths.cert)
	if errors.Is(err, fs.ErrNotExist) {
		return certReissue, nil, nil
	}
	if err != nil {
		return 0, nil, err
	}
	switch {
	case certCovers(c, to.panel, to.admin):
		return certKeep, c, nil
	case felisIssuedCert(c):
		return certReissue, c, nil
	default:
		return certForeign, c, nil
	}
}

func (h domainHost) set(ctx context.Context, arg string, apply bool) (int, error) {
	cfg, err := config.Load(h.paths.hostTOML)
	if err != nil {
		return 1, err
	}
	newRoot, err := normalizeRootDomain(arg)
	if err != nil {
		return 1, err
	}
	cur := effectiveDomainNames(cfg.Server.RootDomain, cfg.Auth.PanelHostname, cfg.Auth.AdminHostname)
	plan := planDomainChange(cur, newRoot)

	// Everything that can refuse runs before anything is written.
	targets, err := h.tomlTargets()
	if err != nil {
		return 1, err
	}
	edited := make(map[string][]byte, len(targets))
	for _, t := range targets {
		raw, err := os.ReadFile(t.real)
		if err != nil {
			return 1, err
		}
		out, err := editTOMLStrings(raw, domainTOMLEdits(plan.to))
		if err != nil {
			return 1, fmt.Errorf("%s: %w; set [server] root_domain and [auth] panel_hostname / admin_hostname there by hand, then run this again", t.path, err)
		}
		if !bytes.Equal(raw, out) {
			edited[t.real] = out
		}
	}
	action, oldCert, err := h.certPlan(plan.to)
	if err != nil {
		return 1, fmt.Errorf("read the panel certificate: %w", err)
	}

	h.printPlan(ctx, plan, action, cfg.SMTP.Host != "")
	if action == certForeign {
		return 1, fmt.Errorf("the panel certificate at %s was not issued by Felis and does not cover %s and %s; install one that does at the same path (key at %s), then run this again",
			h.paths.cert, plan.to.panel, plan.to.admin, h.paths.key)
	}
	if !apply {
		fmt.Fprintf(h.out, "\nNothing was changed. To apply: sudo felis domain set -yes %s\n", plan.to.root)
		return 0, nil
	}

	fmt.Fprintln(h.out, "\nApplying:")
	for _, t := range targets {
		out, ok := edited[t.real]
		if !ok {
			fmt.Fprintf(h.out, "  - %s: already on %s\n", t.path, plan.to.root)
			continue
		}
		info, err := os.Stat(t.real)
		if err != nil {
			return 1, err
		}
		if err := replaceFileKeepingMode(t.real, info, out); err != nil {
			return 1, fmt.Errorf("write %s: %w", t.path, err)
		}
		fmt.Fprintf(h.out, "  - %s: updated\n", t.path)
	}

	if action == certReissue {
		if err := h.reissueCert(plan.to, oldCert); err != nil {
			return 1, err
		}
	} else {
		fmt.Fprintf(h.out, "  - panel certificate: already covers %s and %s\n", plan.to.panel, plan.to.admin)
	}

	secretsChanged, err := h.syncSecrets(ctx, cfg.K8s.Namespace)
	if err != nil {
		return 1, err
	}
	login, err := h.convergeLogin(ctx, cfg, plan.to)
	if err != nil {
		return 1, err
	}
	propsChanged, hostProxy, err := h.syncLinkProps(plan.to)
	if err != nil {
		return 1, err
	}

	roll := secretsChanged
	if !roll {
		live, err := h.liveAPI(ctx, plan.to.panel)
		roll = err != nil || live.names != plan.to || live.cert == nil || !certCovers(live.cert, plan.to.panel, plan.to.admin)
	}
	if roll {
		if err := h.rollAPI(ctx); err != nil {
			return 1, fmt.Errorf("roll felis-api: %w", err)
		}
		fmt.Fprintln(h.out, "  - felis-api: rolled out on the new config and certificate (everyone signs in again)")
	} else {
		fmt.Fprintln(h.out, "  - felis-api: already serving the new names")
	}

	if hostProxy {
		if err := h.restartProxyIfStale(ctx, propsChanged); err != nil {
			return 1, err
		}
	}
	if login {
		h.awaitLogin(ctx, cfg.K8s.Namespace, plan.to)
	}

	fmt.Fprintln(h.out)
	return h.check(ctx), nil
}

func (h domainHost) printPlan(ctx context.Context, p domainPlan, action certAction, smtp bool) {
	if p.from == p.to {
		fmt.Fprintf(h.out, "felis domain set: the install is already on %s; bringing every surface in line with it.\n", p.to.root)
	} else {
		fmt.Fprintf(h.out, "felis domain set: moving the install from %s to %s\n", p.from.root, p.to.root)
	}
	row := func(label, from, to string, custom bool) {
		switch {
		case custom:
			fmt.Fprintf(h.out, "  %-14s %s (set by hand, kept; change [auth] in %s yourself if it should move)\n", label, to, h.paths.hostTOML)
		case from == to:
			fmt.Fprintf(h.out, "  %-14s %s\n", label, to)
		default:
			fmt.Fprintf(h.out, "  %-14s %s → %s\n", label, from, to)
		}
	}
	row("root domain", p.from.root, p.to.root, false)
	row("panel", p.from.panel, p.to.panel, p.customPanel)
	row("admin console", p.from.admin, p.to.admin, p.customAdmin)
	switch action {
	case certKeep:
		fmt.Fprintf(h.out, "  %-14s already covers the names, kept\n", "certificate")
	case certReissue:
		fmt.Fprintf(h.out, "  %-14s reissued for the names (self-signed, as the installer makes it); the old pair is kept beside it\n", "certificate")
	case certForeign:
		fmt.Fprintf(h.out, "  %-14s NOT issued by Felis and does not cover the names\n", "certificate")
	}
	fmt.Fprintln(h.out, "  also: the felis-config and felis-api-tls Secrets, the proxy's felis-link.properties, the login gate's env")
	if p.from == p.to {
		return
	}

	fmt.Fprintln(h.out, "\nWhat the move does:")
	fmt.Fprintf(h.out, "  - DNS: %s, %s, %s and *.%s must reach this host. The *.%s wildcard does not cover %s (a third-level name): it needs its own record.\n",
		p.to.root, p.to.panel, p.to.admin, p.to.root, p.to.root, p.to.admin)
	fmt.Fprintf(h.out, "  - Players reach servers as <name>.%s from now on; the %s addresses stop routing. Restarting the proxy disconnects everyone online.\n", p.to.root, p.from.root)
	fmt.Fprintln(h.out, "  - Everyone signs in again on the new address: sign-in cookies belong to the old hostname.")
	if p.from.panel != p.to.panel {
		switch creds, users, err := h.passkeys(ctx); {
		case err != nil:
			fmt.Fprintf(h.out, "  - Passkeys are bound to %s and stop working on %s (could not count them: %v).\n", p.from.panel, p.to.panel, err)
		case creds > 0:
			fmt.Fprintf(h.out, "  - %d passkey(s) of %d user(s) are bound to %s and stop working on %s: those users sign in with their email code and register a new passkey.\n",
				creds, users, p.from.panel, p.to.panel)
		}
		if !smtp {
			fmt.Fprintln(h.out, "    No [smtp] relay is configured, so email codes are not delivered: an Owner locked out this way recovers with sudo felis breakGlass.")
		}
	}
	if _, err := os.Stat(h.paths.tunnelConfig); err == nil {
		fmt.Fprintln(h.out, "  - The Cloudflare tunnel and Access application still route the old names: re-run the Cloudflare step of sudo felis setup afterwards.")
	}
}

// reissueCert writes a new panel certificate and key for n, keeping the old pair
// beside them.
func (h domainHost) reissueCert(n domainNames, old *x509.Certificate) error {
	var ips []net.IP
	if old != nil {
		ips = old.IPAddresses
	}
	certPEM, keyPEM, err := issuePanelCert(n, ips, h.now())
	if err != nil {
		return fmt.Errorf("issue the panel certificate: %w", err)
	}
	stamp := h.now().UTC().Format("20060102T150405Z")
	kept := ""
	for _, f := range []struct {
		path string
		data []byte
		mode os.FileMode
	}{{h.paths.key, keyPEM, 0o600}, {h.paths.cert, certPEM, 0o644}} {
		info, err := os.Stat(f.path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := os.WriteFile(f.path, f.data, f.mode); err != nil {
				return err
			}
			continue
		case err != nil:
			return err
		}
		prev, err := os.ReadFile(f.path)
		if err != nil {
			return err
		}
		backup := f.path + ".pre-domain-" + stamp
		if err := replaceFileKeepingMode(backup, info, prev); err != nil {
			return fmt.Errorf("keep %s: %w", f.path, err)
		}
		kept = ".pre-domain-" + stamp
		if err := replaceFileKeepingMode(f.path, info, f.data); err != nil {
			return fmt.Errorf("write %s: %w", f.path, err)
		}
	}
	if kept != "" {
		fmt.Fprintf(h.out, "  - panel certificate: reissued for %s and %s (the old pair is kept as *%s)\n", n.panel, n.admin, kept)
	} else {
		fmt.Fprintf(h.out, "  - panel certificate: issued for %s and %s\n", n.panel, n.admin)
	}
	return nil
}

// putSecretKeys sets keys of a Secret, creating it with typ when absent, and
// reports whether anything changed. Keys not named are left alone.
func putSecretKeys(ctx context.Context, cl client.Client, ns, name string, typ corev1.SecretType, data map[string][]byte) (bool, error) {
	var sec corev1.Secret
	err := cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sec)
	if apierrors.IsNotFound(err) {
		return true, cl.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Type: typ, Data: data})
	}
	if err != nil {
		return false, err
	}
	changed := false
	for k, v := range data {
		if !bytes.Equal(sec.Data[k], v) {
			changed = true
		}
	}
	if !changed {
		return false, nil
	}
	if sec.Data == nil {
		sec.Data = map[string][]byte{}
	}
	for k, v := range data {
		sec.Data[k] = v
	}
	return true, cl.Update(ctx, &sec)
}

// syncSecrets puts the pod config and the panel certificate into the Secrets
// felis-api (and the workload-namespace Jobs) mount.
func (h domainHost) syncSecrets(ctx context.Context, minecraftNS string) (bool, error) {
	pod, err := os.ReadFile(h.paths.podTOML)
	if err != nil {
		return false, err
	}
	certPEM, err := os.ReadFile(h.paths.cert)
	if err != nil {
		return false, err
	}
	keyPEM, err := os.ReadFile(h.paths.key)
	if err != nil {
		return false, err
	}
	changedAny := false
	put := func(ns, name string, typ corev1.SecretType, data map[string][]byte) error {
		changed, err := putSecretKeys(ctx, h.cl, ns, name, typ, data)
		if err != nil {
			return fmt.Errorf("write Secret %s/%s: %w", ns, name, err)
		}
		changedAny = changedAny || changed
		state := "unchanged"
		if changed {
			state = "updated"
		}
		fmt.Fprintf(h.out, "  - Secret %s/%s: %s\n", ns, name, state)
		return nil
	}
	for _, ns := range h.configNamespaces(minecraftNS) {
		if err := put(ns, platform.ConfigSecretName, corev1.SecretTypeOpaque, map[string][]byte{platform.ConfigSecretKey: pod}); err != nil {
			return false, err
		}
	}
	if err := put(h.controlNS, platform.APITLSSecretName, corev1.SecretTypeTLS,
		map[string][]byte{corev1.TLSCertKey: certPEM, corev1.TLSPrivateKeyKey: keyPEM}); err != nil {
		return false, err
	}
	return changedAny, nil
}

func (h domainHost) configNamespaces(minecraftNS string) []string {
	ns := []string{h.controlNS}
	if minecraftNS != "" && minecraftNS != h.controlNS {
		ns = append(ns, minecraftNS)
	}
	return ns
}

// convergeLogin sets the config-derived env of the system servers (the login
// gate carries the domain) and nothing else, and reports whether the login gate
// is installed.
func (h domainHost) convergeLogin(ctx context.Context, cfg *config.Config, n domainNames) (bool, error) {
	ns := cfg.K8s.Namespace
	login := false
	for _, p := range systemServerPlans(cfg.Velocity.LoginImage, cfg.Velocity.LobbyImage, platform.InternalAPIBaseURL(h.controlNS), n.root, n.panel) {
		if p.image == "" {
			continue
		}
		desired, err := p.build(p.image, ns)
		if err != nil {
			return false, err
		}
		if len(derivedEnvWanted(desired)) == 0 {
			continue
		}
		var existing v1alpha1.MinecraftServer
		switch err := h.cl.Get(ctx, client.ObjectKeyFromObject(desired), &existing); {
		case apierrors.IsNotFound(err):
			fmt.Fprintf(h.out, "  - %s: not installed, skipped\n", p.name)
			continue
		case err != nil:
			return false, err
		}
		if existing.Labels[v1alpha1.LabelSystemRole] != p.name {
			return false, fmt.Errorf("MinecraftServer %s/%s is not marked as the Felis %q system role; refusing to change it", ns, p.name, p.name)
		}
		var changes []string
		changed, err := patchOnConflictRetry(ctx, h.cl, &existing, func() bool {
			changes = convergeDerivedEnv(&existing, desired)
			return len(changes) > 0
		})
		if err != nil {
			return false, fmt.Errorf("update %s: %w", p.name, err)
		}
		login = login || p.name == naming.SystemLoginServer
		if changed {
			fmt.Fprintf(h.out, "  - %s: updated (%s)\n", p.name, strings.Join(changes, ", "))
		} else {
			fmt.Fprintf(h.out, "  - %s: env already on the new names\n", p.name)
		}
	}
	return login, nil
}

func linkPropsDomain(n domainNames) [][2]string {
	return [][2]string{{"root-domain", n.root}, {"panel-hostname", n.panel}, {"admin-hostname", n.admin}}
}

// readKeyValues reads the named keys of a key=value file, and only those: the
// proxy's file also holds its service token.
func readKeyValues(path string, keys ...string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, ln := range strings.Split(string(raw), "\n") {
		k, v, ok := strings.Cut(ln, "=")
		if k = strings.TrimSpace(k); ok && containsString(keys, k) {
			out[k] = strings.TrimSpace(v)
		}
	}
	return out, nil
}

// syncLinkProps writes the names into the host proxy's felis-link.properties.
// hostProxy is false when the proxy runs elsewhere.
func (h domainHost) syncLinkProps(n domainNames) (changed, hostProxy bool, err error) {
	want := linkPropsDomain(n)
	keys := make([]string, len(want))
	for i, kv := range want {
		keys[i] = kv[0]
	}
	have, err := readKeyValues(h.paths.linkProps, keys...)
	if errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(h.out, "  - %s: not on this host; in your proxy's felis-link.properties set root-domain=%s, panel-hostname=%s, admin-hostname=%s and restart it\n",
			h.paths.linkProps, n.root, n.panel, n.admin)
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	var stale [][2]string
	for _, kv := range want {
		if have[kv[0]] != kv[1] {
			stale = append(stale, kv)
		}
	}
	if len(stale) == 0 {
		fmt.Fprintf(h.out, "  - %s: already on the new names\n", h.paths.linkProps)
		return false, true, nil
	}
	if err := setKeyValueLines(h.paths.linkProps, "=", stale); err != nil {
		return false, true, fmt.Errorf("write %s: %w", h.paths.linkProps, err)
	}
	fmt.Fprintf(h.out, "  - %s: updated\n", h.paths.linkProps)
	return true, true, nil
}

// restartProxyIfStale restarts the host proxy when its config changed or it has
// been running since before the last change.
func (h domainHost) restartProxyIfStale(ctx context.Context, changed bool) error {
	st, err := h.unitState(ctx, velocityUnit)
	if err != nil {
		return err
	}
	if !st.loaded {
		fmt.Fprintf(h.out, "  - %s: no such unit on this host; restart your proxy so it reads felis-link.properties\n", velocityUnit)
		return nil
	}
	if !st.active {
		fmt.Fprintf(h.out, "  - %s: not running; it reads the new names when it starts\n", velocityUnit)
		return nil
	}
	if !changed {
		info, err := os.Stat(h.paths.linkProps)
		if err != nil {
			return err
		}
		if !st.since.IsZero() && !st.since.Before(info.ModTime()) {
			fmt.Fprintf(h.out, "  - %s: already running on the new names\n", velocityUnit)
			return nil
		}
	}
	if err := h.restartUnit(ctx, velocityUnit); err != nil {
		return fmt.Errorf("restart %s: %w", velocityUnit, err)
	}
	fmt.Fprintf(h.out, "  - %s: restarted (players online were disconnected and reconnect on the new addresses)\n", velocityUnit)
	return nil
}

// loginPodState reports whether the login gate's pod runs with n and is Ready.
func (h domainHost) loginPodState(ctx context.Context, ns string, n domainNames) (envOK, ready bool, err error) {
	var pod corev1.Pod
	if err := h.cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: naming.SystemLoginServer + "-0"}, &pod); err != nil {
		return false, false, err
	}
	env := map[string]string{}
	for _, c := range pod.Spec.Containers {
		if c.Name == "minecraft" {
			for _, e := range c.Env {
				env[e.Name] = e.Value
			}
		}
	}
	envOK = env[envRootDomain] == n.root && env[envPanelHostname] == n.panel
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			ready = c.Status == corev1.ConditionTrue
		}
	}
	return envOK, ready, nil
}

// awaitLogin waits for the operator to restart the login gate onto the new env.
func (h domainHost) awaitLogin(ctx context.Context, ns string, n domainNames) {
	deadline := h.now().Add(h.loginWait)
	for {
		if envOK, ready, err := h.loginPodState(ctx, ns, n); err == nil && envOK && ready {
			fmt.Fprintln(h.out, "  - login gate: running on the new names")
			return
		}
		if !h.now().Before(deadline) {
			fmt.Fprintf(h.out, "  - login gate: not running on the new names after %s (the check below says where it stands)\n", h.loginWait)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(h.pollEvery):
		}
	}
}

// domainCheck is one surface's line in `felis domain check`.
type domainCheck struct {
	status  string // ok, FAIL, warn, or "-" (not on this host)
	surface string
	detail  string
}

func (h domainHost) check(ctx context.Context) int {
	cfg, err := config.Load(h.paths.hostTOML)
	if err != nil {
		fmt.Fprintf(h.out, "felis domain check: %v\n", err)
		return 1
	}
	want := effectiveDomainNames(cfg.Server.RootDomain, cfg.Auth.PanelHostname, cfg.Auth.AdminHostname)
	fmt.Fprintf(h.out, "felis domain check: every surface against %s (panel %s, admin console %s)\n", want.root, want.panel, want.admin)
	checks := h.checks(ctx, cfg, want)
	failed := 0
	for _, c := range checks {
		fmt.Fprintf(h.out, "  %-4s %s: %s\n", c.status, c.surface, c.detail)
		if c.status == "FAIL" {
			failed++
		}
	}
	if failed > 0 {
		fmt.Fprintf(h.out, "%d surface(s) are behind; sudo felis domain set %s converges what it owns, and each line above says what else to do.\n", failed, want.root)
		return 1
	}
	fmt.Fprintf(h.out, "Every surface is on %s.\n", want.root)
	return 0
}

func namesDetail(n domainNames) string {
	return fmt.Sprintf("root %s, panel %s, admin %s", n.root, n.panel, n.admin)
}

func (h domainHost) checks(ctx context.Context, cfg *config.Config, want domainNames) []domainCheck {
	var out []domainCheck
	add := func(status, surface, format string, a ...any) {
		out = append(out, domainCheck{status, surface, fmt.Sprintf(format, a...)})
	}
	match := func(surface string, got domainNames) {
		if got == want {
			add("ok", surface, "on the names")
		} else {
			add("FAIL", surface, "has %s", namesDetail(got))
		}
	}

	targets, err := h.tomlTargets()
	if err != nil {
		add("FAIL", "config files", "%v", err)
	}
	for _, t := range targets {
		raw, err := os.ReadFile(t.real)
		if err == nil {
			var got domainNames
			if got, err = tomlDomainNames(raw); err == nil {
				match(t.path, got)
				continue
			}
		}
		add("FAIL", t.path, "%v", err)
	}

	for _, ns := range h.configNamespaces(cfg.K8s.Namespace) {
		surface := "Secret " + ns + "/" + platform.ConfigSecretName
		var sec corev1.Secret
		if err := h.cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: platform.ConfigSecretName}, &sec); err != nil {
			add("FAIL", surface, "%v", err)
			continue
		}
		got, err := tomlDomainNames(sec.Data[platform.ConfigSecretKey])
		if err != nil {
			add("FAIL", surface, "%v", err)
			continue
		}
		match(surface, got)
	}

	certOK := false
	switch c, err := readCertFile(h.paths.cert); {
	case err != nil:
		add("FAIL", "panel certificate", "%v", err)
	case !certCovers(c, want.panel, want.admin):
		add("FAIL", "panel certificate", "%s names %s, not both %s and %s", h.paths.cert, strings.Join(c.DNSNames, ", "), want.panel, want.admin)
	default:
		certOK = true
		add("ok", "panel certificate", "covers both names, valid until %s", c.NotAfter.UTC().Format("2006-01-02"))
	}
	var tlsSec corev1.Secret
	certPEM, _ := os.ReadFile(h.paths.cert)
	keyPEM, _ := os.ReadFile(h.paths.key)
	switch err := h.cl.Get(ctx, client.ObjectKey{Namespace: h.controlNS, Name: platform.APITLSSecretName}, &tlsSec); {
	case err != nil:
		add("FAIL", "Secret "+h.controlNS+"/"+platform.APITLSSecretName, "%v", err)
	case !bytes.Equal(tlsSec.Data[corev1.TLSCertKey], certPEM) || !bytes.Equal(tlsSec.Data[corev1.TLSPrivateKeyKey], keyPEM):
		add("FAIL", "Secret "+h.controlNS+"/"+platform.APITLSSecretName, "differs from %s / %s", h.paths.cert, h.paths.key)
	default:
		add("ok", "Secret "+h.controlNS+"/"+platform.APITLSSecretName, "matches the certificate files")
	}

	switch live, err := h.liveAPI(ctx, want.panel); {
	case err != nil:
		add("FAIL", "felis-api", "%v", err)
	case live.names != want:
		add("FAIL", "felis-api", "serves %s (still on the old config: roll it)", namesDetail(live.names))
	case live.cert == nil || !certCovers(live.cert, want.panel, want.admin):
		add("FAIL", "felis-api", "serves the names but a certificate that does not cover them (roll it after the Secret is right)")
	case certOK && !bytes.Equal(live.cert.Raw, mustCertDER(certPEM)):
		add("warn", "felis-api", "serves the names with a certificate other than %s", h.paths.cert)
	default:
		add("ok", "felis-api", "serves the names and a certificate that covers them")
	}

	h.checkProxy(ctx, want, add)

	var ms v1alpha1.MinecraftServer
	switch err := h.cl.Get(ctx, client.ObjectKey{Namespace: cfg.K8s.Namespace, Name: naming.SystemLoginServer}, &ms); {
	case apierrors.IsNotFound(err):
		add("-", "login gate", "not installed")
	case err != nil:
		add("FAIL", "login gate", "%v", err)
	default:
		env := map[string]string{}
		for _, e := range ms.Spec.Env {
			env[e.Name] = e.Value
		}
		if env[envRootDomain] != want.root || env[envPanelHostname] != want.panel {
			add("FAIL", "login gate", "the MinecraftServer env has %s=%q, %s=%q", envRootDomain, env[envRootDomain], envPanelHostname, env[envPanelHostname])
			break
		}
		switch envOK, ready, err := h.loginPodState(ctx, cfg.K8s.Namespace, want); {
		case err != nil:
			add("FAIL", "login gate", "env is right; its pod: %v", err)
		case !envOK:
			add("FAIL", "login gate", "env is right but the pod still runs the old one (the operator has not restarted it yet)")
		case !ready:
			add("warn", "login gate", "the pod has the new env and is not Ready yet")
		default:
			add("ok", "login gate", "env and running pod on the names")
		}
	}

	h.checkTunnel(want, add)
	h.checkDNS(ctx, want, add)
	return out
}

func mustCertDER(certPEM []byte) []byte {
	if b, _ := pem.Decode(certPEM); b != nil {
		return b.Bytes
	}
	return nil
}

func (h domainHost) checkProxy(ctx context.Context, want domainNames, add func(status, surface, format string, a ...any)) {
	keys := []string{"root-domain", "panel-hostname", "admin-hostname"}
	have, err := readKeyValues(h.paths.linkProps, keys...)
	if errors.Is(err, fs.ErrNotExist) {
		add("-", "proxy", "no felis-link.properties on this host; a proxy elsewhere needs root-domain=%s, panel-hostname=%s, admin-hostname=%s", want.root, want.panel, want.admin)
		return
	}
	if err != nil {
		add("FAIL", "proxy", "%v", err)
		return
	}
	for _, kv := range linkPropsDomain(want) {
		if have[kv[0]] != kv[1] {
			add("FAIL", "proxy", "%s has %s=%q", h.paths.linkProps, kv[0], have[kv[0]])
			return
		}
	}
	info, err := os.Stat(h.paths.linkProps)
	if err != nil {
		add("FAIL", "proxy", "%v", err)
		return
	}
	switch st, err := h.unitState(ctx, velocityUnit); {
	case err != nil:
		add("warn", "proxy", "felis-link.properties is right; could not ask systemd about %s: %v", velocityUnit, err)
	case !st.loaded:
		add("warn", "proxy", "felis-link.properties is right; no %s unit here, so restart your proxy if it has not been", velocityUnit)
	case !st.active:
		add("FAIL", "proxy", "felis-link.properties is right; %s is not running", velocityUnit)
	case st.since.IsZero():
		add("warn", "proxy", "felis-link.properties is right; could not tell when %s started", velocityUnit)
	case st.since.Before(info.ModTime()):
		add("FAIL", "proxy", "%s has run since before felis-link.properties changed: sudo systemctl restart %s", velocityUnit, velocityUnit)
	default:
		add("ok", "proxy", "felis-link.properties on the names, %s started after it changed", velocityUnit)
	}
}

func (h domainHost) checkTunnel(want domainNames, add func(status, surface, format string, a ...any)) {
	raw, err := os.ReadFile(h.paths.tunnelConfig)
	if errors.Is(err, fs.ErrNotExist) {
		add("-", "Cloudflare tunnel", "not set up on this host")
		return
	}
	if err != nil {
		add("FAIL", "Cloudflare tunnel", "%v", err)
		return
	}
	var tc struct {
		Ingress []struct {
			Hostname string `json:"hostname"`
		} `json:"ingress"`
	}
	if err := yaml.Unmarshal(raw, &tc); err != nil {
		add("FAIL", "Cloudflare tunnel", "%s: %v", h.paths.tunnelConfig, err)
		return
	}
	var routed []string
	for _, r := range tc.Ingress {
		if r.Hostname != "" {
			routed = append(routed, r.Hostname)
		}
	}
	for _, host := range []string{want.panel, want.admin} {
		if !containsString(routed, host) {
			add("FAIL", "Cloudflare tunnel", "routes %s, not %s: re-run the Cloudflare step of sudo felis setup", strings.Join(routed, ", "), host)
			return
		}
	}
	add("ok", "Cloudflare tunnel", "routes both names")
}

func (h domainHost) checkDNS(ctx context.Context, want domainNames, add func(status, surface, format string, a ...any)) {
	var missing []string
	for _, host := range []string{want.root, want.panel, want.admin, dnsProbeLabel + "." + want.root} {
		lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := h.lookupHost(lctx, host)
		cancel()
		if err != nil {
			if strings.HasPrefix(host, dnsProbeLabel+".") {
				host = "*." + want.root
			}
			missing = append(missing, host)
		}
	}
	if len(missing) > 0 {
		verb := "does not resolve"
		if len(missing) > 1 {
			verb = "do not resolve"
		}
		// The admin name is the one a wildcard-only zone misses; say why only then.
		note := ""
		if containsString(missing, want.admin) && !containsString(missing, "*."+want.root) {
			note = fmt.Sprintf(": the *.%s wildcard does not cover %s, which needs its own record", want.root, want.admin)
		}
		add("warn", "DNS", "%s %s from this host%s", strings.Join(missing, ", "), verb, note)
		return
	}
	add("ok", "DNS", "the root, both hostnames and *.%s resolve", want.root)
}
