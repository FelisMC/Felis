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

// The file is the pure core, with no bubbletea, so it is unit tested against a controller-runtime fake client; tui_halt.go only calls into it.

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

// Before initial reconciliation, use desiredState when lifecycle phase is absent.
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

// Patch spec only; stopping an already stopped server reports a no-op.
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

// Warn before stopping system servers; recovery access still permits the operation.
func isSystemServer(name string) bool {
	return name == naming.SystemLoginServer || name == naming.SystemLobbyServer
}

// Audit failures do not block recovery halts; return them for display.
// accountable is the OS user who escalated to root: attribution, not proof.
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
