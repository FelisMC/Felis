// Package metrics defines and registers the named felis_* Prometheus
// collectors mandated by spec §23.
//
// The collectors are package-level vars so any subsystem (operator, reaper,
// image builder, prober) can record into them without importing back into a
// metrics owner and risking an import cycle. Wiring them into a registry is a
// single Register call that takes a prometheus.Registerer — the same
// inject-the-interface, fake-at-the-edge pattern used elsewhere in Felis
// (ownerStore, PVCResolver): production passes controller-runtime's global
// Registry (served by the operator's :metrics endpoint), tests pass a fresh
// prometheus.NewRegistry() so assertions never collide with global state.
package metrics

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
)

// namespace prefixes every collector, so the exposed names are exactly
// felis_<name> — matching the metric names spec §23 mandates.
const namespace = "felis"

var (
	// ServersTotal is the current number of MinecraftServers the operator knows
	// about, partitioned by desiredState. It is a gauge, not a monotonic counter:
	// the reconcile loop Set()s it to the live fleet size, so it can fall as
	// servers are deleted. (The _total suffix follows the name spec §23 fixed.)
	ServersTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "servers_total",
		Help:      "Current number of Minecraft servers known to the operator, by desired state.",
	}, []string{"state"})

	// StartDurationSeconds observes the wall-clock time from desiredState=Running
	// to a server reporting ready. Buckets are tuned for Minecraft cold starts
	// (seconds to a few minutes), not the default sub-second web-latency buckets.
	StartDurationSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "start_duration_seconds",
		Help:      "Time from desiredState=Running to a server reporting ready, in seconds.",
		Buckets:   []float64{1, 2, 5, 10, 20, 30, 45, 60, 90, 120, 180, 300, 600},
	})

	// ImageBuildFailuresTotal counts modpack/image build failures (spec §8 lane).
	ImageBuildFailuresTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "image_build_failures_total",
		Help:      "Total number of image build failures.",
	})

	// ReaperWorldsDeletedTotal counts world volumes deleted by the reaper.
	ReaperWorldsDeletedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "reaper_worlds_deleted_total",
		Help:      "Total number of world volumes deleted by the reaper.",
	})

	// OTPLockoutsTotal counts accounts whose email-code door locked after the
	// daily wrong-code budget was spent, by code purpose. Outside a person
	// fumbling codes, a lockout means someone is guessing at that account.
	OTPLockoutsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "auth_otp_lockouts_total",
		Help:      "Email-code doors locked after too many wrong codes, by purpose.",
	}, []string{"purpose"})

	// MailTotal counts mail the API tried to send, by kind (otp, notice) and
	// result: sent, failed (the relay refused it) or throttled (the
	// install-wide mail budget refused it before it reached the relay).
	MailTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "mail_total",
		Help:      "Mail the API tried to send, by kind and result (sent, failed, throttled).",
	}, []string{"kind", "result"})

	// RateLimitedTotal counts requests refused by a volumetric limit, by scope
	// (auth_door: one client address calling the public sign-in doors too fast).
	RateLimitedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "rate_limited_total",
		Help:      "Requests refused by a volumetric rate limit, by scope.",
	}, []string{"scope"})

	// AuthFailuresTotal counts refused sign-in attempts by door (login_email,
	// op_login, passkey, passkey_discoverable, setup_redeem, bind_redeem) and
	// reason (bad_code, no_account, staff_account, bad_credential, ...). A few a
	// day is people mistyping; a steady stream is guessing or enumeration.
	AuthFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "auth_failures_total",
		Help:      "Refused sign-in attempts, by door and reason.",
	}, []string{"door", "reason"})

	// SessionsRevokedTotal counts sessions ended before expiry, by who ended
	// them: logout (the holder) or admin (the owner revoking a user's sessions).
	SessionsRevokedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "sessions_revoked_total",
		Help:      "Sessions revoked before expiry, by who revoked them.",
	}, []string{"by"})

	// AuditWriteFailuresTotal counts audit rows the API failed to write. The
	// action went through; only its record was lost.
	AuditWriteFailuresTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "audit_write_failures_total",
		Help:      "Audit rows the API failed to write.",
	})
)

// OTPPurposes are the email-code doors OTPLockoutsTotal is labelled by.
var OTPPurposes = []string{"onboard_email", "login_email", "op_login", "migrate_confirm"}

// The sign-in alerts watch these counters with increase(). A labelled child
// that does not exist yet has no sample before its first event, so increase()
// would miss exactly the first lockout or throttle; every child the alerts use
// is created at zero up front.
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

// SyncServerGauge republishes felis_servers_total from a full snapshot of the
// fleet's per-server states. states holds one entry per MinecraftServer the
// operator knows about (its desiredState).
//
// It Resets the GaugeVec before Setting one child per distinct state, so a state
// that has drained to zero reports 0 rather than its stale last value. That is
// the whole reason a periodic full-snapshot is used instead of inc/dec on
// reconcile transitions: a snapshot is self-correcting and cannot drift on a
// missed event. Producing the states slice (a cached List of MinecraftServers)
// is the untestable I/O edge; this Reset+tally+Set logic is pure and unit-tested.
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

// Collectors returns every felis_* collector in a stable order. Production and
// tests register the same slice, so the test asserting the full set is exposed
// also pins the production surface.
func Collectors() []prometheus.Collector {
	return []prometheus.Collector{
		ServersTotal,
		StartDurationSeconds,
		ImageBuildFailuresTotal,
		ReaperWorldsDeletedTotal,
		OTPLockoutsTotal,
		MailTotal,
		RateLimitedTotal,
		AuthFailuresTotal,
		SessionsRevokedTotal,
		AuditWriteFailuresTotal,
	}
}

// Register wires every felis_* collector into r.
//
// It is idempotent: re-registering an already-registered collector (a manager
// restart in-process, a test that calls Register twice) is tolerated rather
// than fatal, so a benign double-call never takes the operator down. Any other
// registration error is returned to the caller to surface at startup.
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
