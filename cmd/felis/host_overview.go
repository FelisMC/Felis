package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/dbbackup"
	"felis.lolicon.best/internal/offsite"
	"felis.lolicon.best/internal/watchdog"
)

// Host overview data must not contain secrets.
type hostOverview struct {
	Host    string    `json:"host"`
	At      time.Time `json:"at"`
	Version string    `json:"version"`

	Disks  []hostDisk  `json:"disks"`
	Memory *hostMemory `json:"memory,omitempty"`
	Units  []hostUnit  `json:"units"`
	Proxy  hostProxy   `json:"proxy"`

	Alerts      []hostAlert `json:"alerts"`
	AlertsError string      `json:"alertsError,omitempty"`
	QuietUntil  *time.Time  `json:"quietUntil,omitempty"`
	// Ignored lists every key the owner ignores, firing or not, so each can be restored.
	Ignored []string `json:"ignored"`

	DatabaseBackups      []hostBundle `json:"databaseBackups"`
	DatabaseBackupsError string       `json:"databaseBackupsError,omitempty"`
	Offsite              hostOffsite  `json:"offsite"`

	Settings   hostSettingsView `json:"settings"`
	Connection hostConnection   `json:"connection"`
	Storage    hostStorage      `json:"storage"`
	RootDomain string           `json:"rootDomain"`
}

type hostDisk struct {
	Path      string `json:"path"`
	Total     uint64 `json:"total"`
	Available uint64 `json:"available"`
}

type hostMemory struct {
	Total     uint64 `json:"total"`
	Available uint64 `json:"available"`
}

type hostUnit struct {
	Name   string `json:"name"`
	Active string `json:"active"`
	// Next and Last are a timer's next and last trigger.
	Next *time.Time `json:"next,omitempty"`
	Last *time.Time `json:"last,omitempty"`
}

type hostProxy struct {
	Address   string `json:"address,omitempty"`
	Reachable *bool  `json:"reachable,omitempty"`
}

type hostAlert struct {
	watchdog.Finding
	FirstSeen time.Time  `json:"firstSeen"`
	Notified  *time.Time `json:"notified,omitempty"`
	Ignored   bool       `json:"ignored,omitempty"`
}

type hostBundle struct {
	Name    string    `json:"name"`
	Label   string    `json:"label"`
	Size    int64     `json:"size"`
	Created time.Time `json:"created"`
}

type hostOffsite struct {
	Configured bool   `json:"configured"`
	Endpoint   string `json:"endpoint,omitempty"`
	Bucket     string `json:"bucket,omitempty"`
	Region     string `json:"region,omitempty"`
	Prefix     string `json:"prefix,omitempty"`
	DBKeep     int    `json:"dbKeep,omitempty"`
	// KeyKept says offsite.env holds the key that seals the copies.
	KeyKept bool            `json:"keyKept"`
	Status  *offsite.Status `json:"status,omitempty"`
	Error   string          `json:"error,omitempty"`
}

type hostSettingsView struct {
	// DBBackupTime is the daily backup's HH:MM, empty when the timer says something else.
	DBBackupTime string `json:"dbBackupTime"`
	DBBackupKeep int    `json:"dbBackupKeep"`
	VelocityXmx  string `json:"velocityXmx"`
}

type hostConnection struct {
	// Mode is cloudflare (an Access audience is set), reverse-proxy (the client address
	// comes in X-Forwarded-For) or local.
	Mode      string `json:"mode"`
	PanelHost string `json:"panelHost"`
	AdminHost string `json:"adminHost"`
}

type hostStorage struct {
	Mode     string `json:"mode"`
	Endpoint string `json:"endpoint,omitempty"`
	Bucket   string `json:"bucket,omitempty"`
	Region   string `json:"region,omitempty"`
}

func (e nodeExecutor) overview(ctx context.Context) (any, error) {
	env, err := hostStatusEnv(systemdUnitDir)
	if err != nil {
		return nil, err
	}
	v := hostOverview{Host: env.host, At: env.now.UTC(), Version: resolvedVersion(), Units: []hostUnit{}, Alerts: []hostAlert{}, DatabaseBackups: []hostBundle{}}

	seen := map[uint64]bool{}
	for _, p := range splitList(env.w.diskPaths) {
		var st syscall.Stat_t
		if syscall.Stat(p, &st) != nil || seen[uint64(st.Dev)] {
			continue
		}
		seen[uint64(st.Dev)] = true
		var fs syscall.Statfs_t
		if syscall.Statfs(p, &fs) != nil || fs.Blocks == 0 {
			continue
		}
		bsize := uint64(fs.Bsize)
		v.Disks = append(v.Disks, hostDisk{Path: p, Total: uint64(fs.Blocks) * bsize, Available: uint64(fs.Bavail) * bsize})
	}
	if total, avail, ok := readMeminfo(env.meminfo); ok {
		v.Memory = &hostMemory{Total: total, Available: avail}
	}
	v.Units = hostUnits(ctx, env)

	if env.w.proxyAddr != "" {
		reachable := false
		if conn, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "tcp", env.w.proxyAddr); err == nil {
			conn.Close()
			reachable = true
		}
		v.Proxy = hostProxy{Address: env.w.proxyAddr, Reachable: &reachable}
	}

	ignored, err := watchdog.LoadIgnored(env.w.ignorePath)
	if err != nil {
		v.AlertsError = err.Error()
	}
	v.Ignored = []string{}
	for key := range ignored {
		v.Ignored = append(v.Ignored, key)
	}
	sort.Strings(v.Ignored)
	if state, err := watchdog.LoadState(watchdog.NewestState(env.w.statePath, env.w.fallbackState)); err != nil {
		v.AlertsError = err.Error()
	} else {
		for _, a := range state.Alerts {
			if !a.ClearedAt.IsZero() {
				continue
			}
			alert := hostAlert{Finding: a.Finding, FirstSeen: a.FirstSeen, Ignored: ignored[a.Key]}
			if !a.Notified.IsZero() {
				n := a.Notified
				alert.Notified = &n
			}
			v.Alerts = append(v.Alerts, alert)
		}
		sort.Slice(v.Alerts, func(i, j int) bool { return v.Alerts[i].Key < v.Alerts[j].Key })
	}
	if until := watchdog.QuietUntil(env.w.quietPath); until.After(env.now) {
		v.QuietUntil = &until
	}

	if bundles, err := dbbackup.List(dbbackup.DefaultDir); err != nil {
		v.DatabaseBackupsError = err.Error()
	} else {
		for _, b := range bundles {
			v.DatabaseBackups = append(v.DatabaseBackups, hostBundle{Name: b.Name, Label: b.Label, Size: b.Size, Created: b.Created})
		}
	}

	o := env.cfg.Offsite
	v.Offsite = hostOffsite{Configured: o.Enabled(), Endpoint: o.Endpoint, Bucket: o.Bucket, Region: o.Region, Prefix: o.Prefix, DBKeep: o.DBKeep}
	if _, err := offsiteKey(); err == nil {
		v.Offsite.KeyKept = true
	}
	if o.Enabled() {
		if st, err := offsite.ReadStatus(env.w.offsiteStatus); err != nil {
			v.Offsite.Error = err.Error()
		} else {
			v.Offsite.Status = st
		}
	}

	v.Settings = currentHostSettings(systemdUnitDir)
	v.Connection = connectionOf(env.cfg)
	v.Storage = storageOf(env.cfg)
	v.RootDomain = env.cfg.Server.RootDomain
	return v, nil
}

// hostUnits reads the installer's long-running services and timers: whether each is
// active and, for a timer, when it fires next and last fired.
func hostUnits(ctx context.Context, env statusEnv) []hostUnit {
	var units []hostUnit
	for _, name := range append(append([]string{}, hostServices...), "felis-node-control.service", "cloudflared-felis.service") {
		if _, err := os.Stat(filepath.Join(env.unitDir, name)); err != nil {
			continue
		}
		units = append(units, hostUnit{Name: name, Active: unitActiveState(ctx, doctorEnv{run: env.run}, name)})
	}
	timers, _ := filepath.Glob(filepath.Join(env.unitDir, "felis-*.timer"))
	sort.Strings(timers)
	for _, path := range timers {
		name := filepath.Base(path)
		u := hostUnit{Name: name, Active: unitActiveState(ctx, doctorEnv{run: env.run}, name)}
		show, _ := env.run(ctx, "systemctl", "show", "--timestamp=unix", "-p", "NextElapseUSecRealtime", "-p", "LastTriggerUSec", name)
		for _, ln := range strings.Split(string(show), "\n") {
			k, val, ok := strings.Cut(strings.TrimSpace(ln), "=")
			if !ok {
				continue
			}
			sec, _ := strconv.ParseInt(strings.TrimPrefix(val, "@"), 10, 64)
			if sec <= 0 {
				continue
			}
			t := time.Unix(sec, 0).UTC()
			switch k {
			case "NextElapseUSecRealtime":
				u.Next = &t
			case "LastTriggerUSec":
				u.Last = &t
			}
		}
		units = append(units, u)
	}
	return units
}

// currentHostSettings reads the settings the panel can change from the units the
// installer wrote, which is what the host runs whatever host-settings.env says.
func currentHostSettings(unitDir string) hostSettingsView {
	var v hostSettingsView
	if raw, err := os.ReadFile(filepath.Join(unitDir, "felis-db-backup.timer")); err == nil {
		for _, ln := range strings.Split(string(raw), "\n") {
			if cal, ok := strings.CutPrefix(strings.TrimSpace(ln), "OnCalendar="); ok {
				if hm, ok := strings.CutPrefix(cal, "*-*-* "); ok && len(hm) == len("03:30:00") && strings.HasSuffix(hm, ":00") {
					v.DBBackupTime = strings.TrimSuffix(hm, ":00")
				}
			}
		}
	}
	if raw, err := os.ReadFile(filepath.Join(unitDir, "felis-db-backup.service")); err == nil {
		fields := strings.Fields(string(raw))
		for i, f := range fields {
			if f == "-keep" && i+1 < len(fields) {
				v.DBBackupKeep, _ = strconv.Atoi(fields[i+1])
			}
		}
	}
	if raw, err := os.ReadFile(filepath.Join(unitDir, "felis-velocity.service")); err == nil {
		if m := unitXmx.FindStringSubmatch(string(raw)); m != nil {
			v.VelocityXmx = m[1]
		}
	}
	return v
}

func connectionOf(cfg *config.Config) hostConnection {
	c := hostConnection{Mode: "local", PanelHost: cfg.Auth.PanelHostname, AdminHost: cfg.Auth.AdminHostname}
	switch {
	case cfg.Auth.AccessJWTAud != "":
		c.Mode = "cloudflare"
	case strings.EqualFold(cfg.Auth.ClientIPHeader, "X-Forwarded-For"):
		c.Mode = "reverse-proxy"
	}
	return c
}

func storageOf(cfg *config.Config) hostStorage {
	base := cfg.Registry.UserUploadsContext
	if !strings.HasPrefix(strings.ToLower(base), "s3://") {
		return hostStorage{Mode: "local"}
	}
	bucket, _, _ := strings.Cut(base[len("s3://"):], "/")
	return hostStorage{Mode: "s3", Endpoint: cfg.Registry.S3.Endpoint, Bucket: bucket, Region: cfg.Registry.S3.Region}
}
