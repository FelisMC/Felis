package operator

// ProbeMissesTracked is how many servers the reconciler holds a probe-miss count
// for, so the external tests can see a count go with its server.
func (r *Reconciler) ProbeMissesTracked() int {
	r.probeMu.Lock()
	defer r.probeMu.Unlock()
	return len(r.probeFailures)
}

// ArrivalWindow is how long after a run's first ready probe a zero tally starts no
// idle countdown.
const ArrivalWindow = arrivalWindow
