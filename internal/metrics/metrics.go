// Package metrics defines and registers the felis_* Prometheus collectors.
package metrics

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
)

// Prefix of every collector name, so exposed names are felis_<name>.
const namespace = "felis"

var (
	// Gauge of MinecraftServers by desiredState; it can fall when servers are deleted.
	ServersTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "servers_total",
		Help:      "Current number of Minecraft servers known to the operator, by desired state.",
	}, []string{"state"})

	// 1 for each server's current phase; role and desired let alerts tell a deliberate stop from a failure.
	ServerPhase = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "server_phase",
		Help:      "1 for each Minecraft server's current phase, by server, system role, phase and desired state.",
	}, []string{"server", "role", "phase", "desired"})

	// Seconds from desiredState=Running to ready; buckets suit Minecraft cold starts (seconds to minutes).
	StartDurationSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "start_duration_seconds",
		Help:      "Time from desiredState=Running to a server reporting ready, in seconds.",
		Buckets:   []float64{1, 2, 5, 10, 20, 30, 45, 60, 90, 120, 180, 300, 600},
	})

	// ImageBuildFailuresTotal counts modpack and image build failures.
	ImageBuildFailuresTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "image_build_failures_total",
		Help:      "Total number of image build failures.",
	})

	ReaperWorldsDeletedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "reaper_worlds_deleted_total",
		Help:      "Total number of world volumes deleted by the reaper.",
	})

	// OTPLockoutsTotal counts email-code lockouts by purpose; a lockout means someone is guessing at an account.
	OTPLockoutsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "auth_otp_lockouts_total",
		Help:      "Email-code doors locked after too many wrong codes, by purpose.",
	}, []string{"purpose"})

	// MailTotal counts mail attempts by kind and result: sent, failed or throttled (install-wide budget).
	MailTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "mail_total",
		Help:      "Mail the API tried to send, by kind and result (sent, failed, throttled).",
	}, []string{"kind", "result"})

	// RateLimitedTotal counts requests rejected by scope.
	RateLimitedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "rate_limited_total",
		Help:      "Requests refused by a volumetric rate limit, by scope.",
	}, []string{"scope"})

	// AuthFailuresTotal counts refused sign-ins by door and reason; a steady stream means guessing or enumeration.
	AuthFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "auth_failures_total",
		Help:      "Refused sign-in attempts, by door and reason.",
	}, []string{"door", "reason"})

	// SessionsRevokedTotal counts sessions ended before expiry, by who ended them: logout, self, security or admin.
	SessionsRevokedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "sessions_revoked_total",
		Help:      "Sessions revoked before expiry, by who revoked them.",
	}, []string{"by"})

	// AuditWriteFailuresTotal indicates completed actions whose audit record could not be written.
	AuditWriteFailuresTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "audit_write_failures_total",
		Help:      "Audit rows the API failed to write.",
	})

	// BuildInfo is 1 for the serving process, labelled by component and version, so absent(felis_build_info{component="api"}) detects a down api.
	BuildInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "build_info",
		Help:      "1 for the Felis component serving these metrics, by component and version.",
	}, []string{"component", "version"})
)

func SetBuildInfo(component, version string) {
	BuildInfo.WithLabelValues(component, version).Set(1)
}

var OTPPurposes = []string{"onboard_email", "login_email", "op_login", "migrate_confirm"}

// Created at zero up front so increase() sees the first event of each label set.
func init() {
	for _, kind := range []string{"otp", "notice"} {
		for _, result := range []string{"sent", "failed", "throttled"} {
			MailTotal.WithLabelValues(kind, result)
		}
	}
	RateLimitedTotal.WithLabelValues("auth_door")
	for _, p := range OTPPurposes {
		OTPLockoutsTotal.WithLabelValues(p)
	}
}

// SyncServerGauge rebuilds felis_servers_total from a full snapshot; Reset drains states that vanished to 0.
func SyncServerGauge(states []string) {
	ServersTotal.Reset()
	counts := make(map[string]int, len(states))
	for _, s := range states {
		counts[s]++
	}
	for state, n := range counts {
		ServersTotal.WithLabelValues(state).Set(float64(n))
	}
}

type ServerPhaseSample struct {
	Server, Role, Phase, Desired string
}

// SyncServerPhases resets before publishing the complete snapshot.
func SyncServerPhases(samples []ServerPhaseSample) {
	ServerPhase.Reset()
	for _, s := range samples {
		ServerPhase.WithLabelValues(s.Server, s.Role, s.Phase, s.Desired).Set(1)
	}
}

// Collectors returns every felis_* collector in a stable order.
func Collectors() []prometheus.Collector {
	return []prometheus.Collector{
		ServersTotal,
		ServerPhase,
		StartDurationSeconds,
		ImageBuildFailuresTotal,
		ReaperWorldsDeletedTotal,
		OTPLockoutsTotal,
		MailTotal,
		RateLimitedTotal,
		AuthFailuresTotal,
		SessionsRevokedTotal,
		AuditWriteFailuresTotal,
		BuildInfo,
	}
}

// Register adds every collector to r; re-registering an already-registered collector is tolerated.
func Register(r prometheus.Registerer) error {
	for _, c := range Collectors() {
		if err := r.Register(c); err != nil {
			var already prometheus.AlreadyRegisteredError
			if errors.As(err, &already) {
				continue
			}
			return err
		}
	}
	return nil
}
