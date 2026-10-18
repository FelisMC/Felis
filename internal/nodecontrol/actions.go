package nodecontrol

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type action struct {
	// Blocking actions hold server starts and world maintenance while running.
	blocking bool
	// confirm requires ConfirmMaintenance; confirmIf restricts that requirement to selected parameters.
	confirm         bool
	confirmIf       func(Request) bool
	timeout         time.Duration
	params, secrets []string
	check           func(Request) error
}

var (
	bundlePattern   = regexp.MustCompile(`^felis-db-[0-9]{8}T[0-9]{6}Z-[a-z][a-z-]*\.tar$`)
	domainPattern   = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
	serverPattern   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	bucketPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	regionPattern   = regexp.MustCompile(`^[a-zA-Z0-9-]{0,64}$`)
	prefixPattern   = regexp.MustCompile(`^[A-Za-z0-9._/-]{0,256}$`)
	xmxPattern      = regexp.MustCompile(`^[1-9][0-9]{0,4}[MG]$`)
	timePattern     = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
	ifacePattern    = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,15}$`)
	accountPattern  = regexp.MustCompile(`^[a-f0-9]{32}$`)
	identityPattern = regexp.MustCompile(`^([A-Za-z0-9._%+-]+)?@[A-Za-z0-9.-]+\.[A-Za-z]{2,63}$`)
	printable       = regexp.MustCompile(`^[\x21\x23-\x26\x28-\x5b\x5d-\x7e]+$`)
	rotatableKinds  = []string{"velocity", "limbo", "build", "ops", "registry", "forwarding", "db"}
)

func nodeAction(check func(Request) error) action {
	return action{blocking: true, confirm: true, timeout: 45 * time.Minute, check: check}
}

var actions = map[string]action{
	"fence":           nodeAction(func(r Request) error { return needNodeName(r) }),
	"unfence":         nodeAction(func(r Request) error { return needNodeName(r) }),
	"enable-ha":       {blocking: true, confirm: true, timeout: 45 * time.Minute, params: []string{"vip", "vipInterface", "vrid"}, check: checkHA},
	"join":            nodeAction(func(r Request) error { return needWorker(r, true) }),
	"approve":         nodeAction(func(r Request) error { return needWorker(r, false) }),
	"upgrade":         nodeAction(func(r Request) error { return needWorker(r, false) }),
	"join-controller": nodeAction(func(r Request) error { return needWorker(r, true) }),
	"enable":          nodeAction(needFixedIP),

	"db-backup": {timeout: 30 * time.Minute},
	"db-verify": {timeout: 10 * time.Minute, params: []string{"bundle"}, check: needBundle},
	"db-delete": {timeout: time.Minute, params: []string{"bundle"}, check: needBundle},
	"db-restore": {blocking: true, confirm: true, timeout: 90 * time.Minute, params: []string{"bundle"},
		check: needBundle},

	"offsite-sync":      {timeout: time.Minute},
	"offsite-list":      {timeout: 5 * time.Minute},
	"offsite-check-key": {timeout: 5 * time.Minute},
	"offsite-take-over": {confirm: true, timeout: 5 * time.Minute},
	"offsite-configure": {timeout: 10 * time.Minute, params: []string{"endpoint", "bucket", "region", "prefix", "dbKeep"},
		secrets: []string{"accessKey", "secretKey", "key"}, check: checkOffsite},
	"offsite-disable": {confirm: true, timeout: 5 * time.Minute},
	"restore-host": {blocking: true, confirm: true, timeout: 12 * time.Hour, params: []string{"bundle", "takeOver", "force"},
		check: checkRestoreHost},

	"backup-now": {timeout: 6 * time.Hour, params: []string{"stop", "servers"},
		confirmIf: func(r Request) bool { return r.Params["stop"] == "true" }, check: checkBackupNow},

	"rotate-token": {blocking: true, confirm: true, timeout: 30 * time.Minute, params: []string{"kind"},
		check: func(r Request) error { return oneOf(r.Params["kind"], rotatableKinds, "credential") }},

	"domain-check": {timeout: 5 * time.Minute},
	"domain-set": {blocking: true, confirm: true, timeout: 30 * time.Minute, params: []string{"domain"},
		check: func(r Request) error {
			if d := r.Params["domain"]; len(d) > 200 || !domainPattern.MatchString(d) {
				return errors.New("the domain must be a DNS name such as example.com")
			}
			return nil
		}},
	"storage-configure": {confirm: true, timeout: 10 * time.Minute, params: []string{"mode", "endpoint", "bucket", "region"},
		secrets: []string{"accessKey", "secretKey"}, check: checkStorage},
	"connection-configure": {confirm: true, timeout: 20 * time.Minute, params: []string{"mode", "panelHost", "adminHost", "identity", "accountID"},
		secrets: []string{"apiToken"}, check: checkConnection},

	// Legacy forwarding trusts identity from any reachable client, so enabling it requires confirmation.
	"server-forwarding": {timeout: 2 * time.Minute, params: []string{"server", "legacy"},
		confirmIf: func(r Request) bool { return r.Params["legacy"] == "true" },
		check: func(r Request) error {
			if !serverPattern.MatchString(r.Params["server"]) {
				return errors.New("invalid server name")
			}
			if v := r.Params["legacy"]; v != "true" && v != "false" {
				return errors.New("parameter \"legacy\" must be true or false")
			}
			return nil
		}},
	"converge": {timeout: 10 * time.Minute, params: []string{"userRcon"},
		confirmIf: func(r Request) bool { return r.Params["userRcon"] == "true" },
		check:     func(r Request) error { return boolParam(r, "userRcon") }},
	"doctor":         {timeout: 3 * time.Minute},
	"support-bundle": {timeout: 10 * time.Minute, params: []string{"serverLogs"}, check: func(r Request) error { return boolParam(r, "serverLogs") }},
	"host-settings": {timeout: 30 * time.Minute, params: []string{"dbBackupTime", "dbBackupKeep", "velocityXmx"},
		confirmIf: func(r Request) bool { return r.Params["velocityXmx"] != "" },
		check:     checkHostSettings},
}

func NodeAction(name string) bool {
	switch name {
	case "fence", "unfence", "enable-ha", "join", "approve", "upgrade", "join-controller", "enable":
		return true
	}
	return false
}

// Blocking treats unknown actions as holding the gate.
func Blocking(name string) bool {
	a, ok := actions[name]
	return !ok || a.blocking
}

func timeoutFor(name string) time.Duration {
	if a, ok := actions[name]; ok && a.timeout > 0 {
		return a.timeout
	}
	return 45 * time.Minute
}

func (r Request) Validate() error {
	a, ok := actions[r.Action]
	if !ok {
		return errors.New("unsupported node operation")
	}
	if (a.confirm || (a.confirmIf != nil && a.confirmIf(r))) && !r.ConfirmMaintenance {
		return errors.New("maintenance impact confirmation is required")
	}
	for k, v := range r.Params {
		if !contains(a.params, k) {
			return fmt.Errorf("unsupported parameter %q", k)
		}
		if len(v) > 4096 {
			return fmt.Errorf("parameter %q is too long", k)
		}
	}
	for k, v := range r.Secrets {
		if !contains(a.secrets, k) {
			return fmt.Errorf("unsupported secret %q", k)
		}
		if v != "" && (len(v) > 1024 || !printable.MatchString(v)) {
			return fmt.Errorf("secret %q must be 1 to 1024 printable characters without spaces, quotes or backslashes", k)
		}
	}
	if a.check != nil {
		if err := a.check(r); err != nil {
			return err
		}
	}
	for _, peer := range r.Peers {
		p, err := netip.ParsePrefix(peer)
		if err != nil || p.Bits() != p.Addr().BitLen() || p.Addr().IsLoopback() || p.Addr().IsMulticast() || p.Addr().IsUnspecified() {
			return errors.New("peer addresses must use exact /32 or /128 prefixes")
		}
	}
	if len(r.Peers) > 100 {
		return errors.New("too many peer addresses")
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func needNodeName(r Request) error {
	if !validNodeName(r.Name) {
		return errors.New("invalid node name")
	}
	return nil
}

func needWorker(r Request, fixedIP bool) error {
	if !validNodeName(r.Name) || !targetPattern.MatchString(r.SSHTarget) {
		return errors.New("invalid worker name or SSH target")
	}
	if fixedIP {
		return needFixedIP(r)
	}
	return nil
}

func needFixedIP(r Request) error {
	if a, err := netip.ParseAddr(r.ExternalIP); err != nil || a.IsUnspecified() || a.IsLoopback() || a.IsMulticast() || a.Zone() != "" {
		return errors.New("a fixed node IP is required")
	}
	return nil
}

func checkHA(r Request) error {
	vip, iface, vrid := r.Params["vip"], r.Params["vipInterface"], r.Params["vrid"]
	if vip == "" {
		if iface != "" || vrid != "" {
			return errors.New("the interface and router ID belong to a floating address")
		}
		return nil
	}
	if a, err := netip.ParseAddr(vip); err != nil || !a.Is4() || a.IsLoopback() || a.IsMulticast() || a.IsUnspecified() {
		return errors.New("the floating address must be an IPv4 address")
	}
	if iface != "" && !ifacePattern.MatchString(iface) {
		return errors.New("invalid network interface name")
	}
	if vrid != "" {
		if n, err := strconv.Atoi(vrid); err != nil || n < 1 || n > 255 {
			return errors.New("the router ID must be 1 to 255")
		}
	}
	return nil
}

func needBundle(r Request) error {
	if !bundlePattern.MatchString(r.Params["bundle"]) {
		return errors.New("name a database backup such as felis-db-20261001T033000Z-daily.tar")
	}
	return nil
}

func boolParam(r Request, keys ...string) error {
	for _, k := range keys {
		if v, ok := r.Params[k]; ok && v != "true" && v != "false" {
			return fmt.Errorf("parameter %q must be true or false", k)
		}
	}
	return nil
}

func oneOf(v string, allowed []string, what string) error {
	if !contains(allowed, v) {
		return fmt.Errorf("unsupported %s %q", what, v)
	}
	return nil
}

func checkEndpoint(raw string) error {
	if !strings.HasPrefix(raw, "https://") && !strings.HasPrefix(raw, "http://") || len(raw) > 512 || strings.ContainsAny(raw, " \"'\\\n") {
		return errors.New("the endpoint must be an http(s) URL")
	}
	return nil
}

func checkOffsite(r Request) error {
	p := r.Params
	if err := checkEndpoint(p["endpoint"]); err != nil {
		return err
	}
	if !bucketPattern.MatchString(p["bucket"]) {
		return errors.New("invalid bucket name")
	}
	if !regionPattern.MatchString(p["region"]) || !prefixPattern.MatchString(p["prefix"]) {
		return errors.New("invalid region or key prefix")
	}
	if v := p["dbKeep"]; v != "" {
		if n, err := strconv.Atoi(v); err != nil || n < 1 || n > 1000 {
			return errors.New("the number of database bundles kept must be 1 to 1000")
		}
	}
	return nil
}

func checkRestoreHost(r Request) error {
	if b := r.Params["bundle"]; b != "" && !bundlePattern.MatchString(b) {
		return errors.New("name a database backup such as felis-db-20261001T033000Z-daily.tar")
	}
	return boolParam(r, "takeOver", "force")
}

func checkBackupNow(r Request) error {
	if err := boolParam(r, "stop"); err != nil {
		return err
	}
	if s := r.Params["servers"]; s != "" {
		names := strings.Split(s, ",")
		if len(names) > 500 {
			return errors.New("too many servers")
		}
		for _, n := range names {
			if !serverPattern.MatchString(n) {
				return fmt.Errorf("invalid server name %q", n)
			}
		}
	}
	return nil
}

func checkStorage(r Request) error {
	switch r.Params["mode"] {
	case "local":
		if len(r.Params) > 1 || len(r.Secrets) > 0 {
			return errors.New("local storage takes no bucket")
		}
		return nil
	case "s3":
		if err := checkEndpoint(r.Params["endpoint"]); err != nil {
			return err
		}
		if !bucketPattern.MatchString(r.Params["bucket"]) || !regionPattern.MatchString(r.Params["region"]) {
			return errors.New("invalid bucket or region")
		}
		if r.Secrets["accessKey"] == "" || r.Secrets["secretKey"] == "" {
			return errors.New("the bucket's access key and secret key are required")
		}
		return nil
	}
	return errors.New("storage mode must be local or s3")
}

func checkConnection(r Request) error {
	host := func(k string) error {
		if v := r.Params[k]; v != "" && (len(v) > 253 || !domainPattern.MatchString(v)) {
			return fmt.Errorf("parameter %q must be a DNS name", k)
		}
		return nil
	}
	if err := host("panelHost"); err != nil {
		return err
	}
	if err := host("adminHost"); err != nil {
		return err
	}
	switch r.Params["mode"] {
	case "local", "reverse-proxy":
		if r.Params["accountID"] != "" || r.Params["identity"] != "" || len(r.Secrets) > 0 {
			return errors.New("only a Cloudflare connection takes an account, an Access identity and a token")
		}
		if r.Params["mode"] == "reverse-proxy" && r.Params["adminHost"] == "" {
			return errors.New("a reverse proxy needs the admin hostname")
		}
		return nil
	case "cloudflare":
		if !accountPattern.MatchString(r.Params["accountID"]) {
			return errors.New("the Cloudflare account ID is 32 hexadecimal characters")
		}
		if r.Params["adminHost"] == "" {
			return errors.New("the admin hostname is required behind Cloudflare Access")
		}
		if id := r.Params["identity"]; !identityPattern.MatchString(id) {
			return errors.New("name who may sign in: an email address, or @ and an email domain")
		}
		if r.Secrets["apiToken"] == "" {
			return errors.New("a Cloudflare API token is required")
		}
		return nil
	}
	return errors.New("connection mode must be local, reverse-proxy or cloudflare")
}

func checkHostSettings(r Request) error {
	p := r.Params
	if len(p) == 0 {
		return errors.New("no setting to change")
	}
	if v := p["dbBackupTime"]; v != "" && !timePattern.MatchString(v) {
		return errors.New("the backup time must be HH:MM")
	}
	if v := p["dbBackupKeep"]; v != "" {
		if n, err := strconv.Atoi(v); err != nil || n < 1 || n > 365 {
			return errors.New("the number of daily backups kept must be 1 to 365")
		}
	}
	if v := p["velocityXmx"]; v != "" {
		n, _ := strconv.Atoi(strings.TrimRight(v, "MG"))
		if !xmxPattern.MatchString(v) || (strings.HasSuffix(v, "M") && n < 256) || (strings.HasSuffix(v, "G") && n > 64) {
			return errors.New("the proxy heap must be 256M to 64G, written like 1G or 768M")
		}
	}
	return nil
}
