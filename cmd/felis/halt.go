package main

import (
	"context"
	"encoding/json"
	"fmt"

	"felis.lolicon.best/internal/api"
	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/naming"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// halt is the break-glass "stop a running server now" op (#31). Its authority is
// the same as the rest of the console: local root holding the cluster kubeconfig.
// The console does not stop the pod itself — it flips the MinecraftServer's desired
// state to Stopped and lets the operator reconcile that into a graceful shutdown, so
// a halt is exactly the CRD write the operator already knows how to honour.
//
// This file is the pure core — no bubbletea, no huh — so the whole thing is unit
// tested against a controller-runtime fake client. The TUI shell lives in
// tui_halt.go and only calls into here.

// haltableServer is a MinecraftServer projected down to what the halt picker shows:
// its name, its observed phase (or desired state before the operator has reconciled
// it), and whether it is a platform system server whose halt takes the front door
// down.
type haltableServer struct {
	name   string
	phase  string
	system bool
}

// haltOutcome is the durable result of a halt attempt, surfaced in the TUI card and
// re-printed to the normal screen after the alt-screen tears down.
type haltOutcome struct {
	name           string
	namespace      string
	alreadyStopped bool
	system         bool  // login/lobby — halting these takes the auth front door down
	auditErr       error // non-nil if the accountability row could not be written
}

// listServersForHalt lists the MinecraftServers an operator may halt in the given
// namespace, projecting only what the picker renders. The phase falls back to the
// desired state for a server the operator has not yet reconciled (empty status).
func listServersForHalt(ctx context.Context, cl client.Client, namespace string) ([]haltableServer, error) {
	var list v1alpha1.MinecraftServerList
	if err := cl.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	out := make([]haltableServer, 0, len(list.Items))
	for i := range list.Items {
		ms := &list.Items[i]
		phase := string(ms.Status.Phase)
		if phase == "" {
			phase = string(ms.Spec.DesiredState) // not yet reconciled — show intent
		}
		out = append(out, haltableServer{
			name:   ms.Name,
			phase:  phase,
			system: isSystemServer(ms.Name),
		})
	}
	return out, nil
}

// haltServer flips one MinecraftServer's desiredState to Stopped with a spec-only
// merge patch. MergeFrom (not Update) is deliberate: the operator writes status on
// the same object continuously, and a full-object Update would race and clobber it,
// whereas a merge patch of spec.desiredState touches a disjoint field. Halting a
// server already Stopped is a no-op, reported via alreadyStopped so the console can
// say "already stopped" instead of claiming it just stopped it.
func haltServer(ctx context.Context, cl client.Client, namespace, name string) (haltOutcome, error) {
	var ms v1alpha1.MinecraftServer
	if err := cl.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &ms); err != nil {
		if apierrors.IsNotFound(err) {
			return haltOutcome{}, fmt.Errorf("no MinecraftServer %q in namespace %q", name, namespace)
		}
		return haltOutcome{}, fmt.Errorf("get server %q: %w", name, err)
	}
	out := haltOutcome{name: name, namespace: namespace, system: isSystemServer(name)}
	if ms.Spec.DesiredState == v1alpha1.DesiredStopped {
		out.alreadyStopped = true
		return out, nil
	}
	patch := client.MergeFrom(ms.DeepCopy())
	ms.Spec.DesiredState = v1alpha1.DesiredStopped
	if err := cl.Patch(ctx, &ms, patch); err != nil {
		return haltOutcome{}, fmt.Errorf("halt server %q: %w", name, err)
	}
	return out, nil
}

// isSystemServer reports whether name is a platform-provisioned system server. The
// login gate has no fallback (systemservers.go), so halting it locks every player
// out of the whole proxy — the console warns when the target is one rather than
// forbidding it, since break-glass is deliberately full power.
func isSystemServer(name string) bool {
	return name == naming.SystemLoginServer || name == naming.SystemLobbyServer
}

// performHalt runs haltServer and records a best-effort accountability audit row. It
// mirrors performBreakGlass: the halt succeeds even when the audit sink is unhappy
// (break-glass must work with logging down), and any audit error rides back in the
// outcome for the console to surface. accountable is the OS user who escalated to
// root — attribution, not proof (the root gate is the real authority).
func performHalt(ctx context.Context, cl client.Client, s ownerStore, namespace, name, accountable string) (haltOutcome, error) {
	out, err := haltServer(ctx, cl, namespace, name)
	if err != nil {
		return haltOutcome{}, err
	}
	out.auditErr = auditHalt(ctx, s, out, accountable)
	return out, nil
}

// auditHalt writes the halt accountability row under the break_glass.halt action,
// recording who halted what and whether it was a no-op or a system server.
func auditHalt(ctx context.Context, s ownerStore, out haltOutcome, accountable string) error {
	blob, err := json.Marshal(map[string]any{
		"server":          out.name,
		"namespace":       out.namespace,
		"os_user":         accountable,
		"already_stopped": out.alreadyStopped,
		"system_server":   out.system,
	})
	if err != nil {
		return err
	}
	return s.Audit(ctx, api.AuditEntry{
		Actor:   accountable,
		Source:  "break-glass",
		Action:  "break_glass.halt",
		Payload: blob,
	})
}
