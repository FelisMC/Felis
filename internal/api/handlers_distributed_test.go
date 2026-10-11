package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"felis.lolicon.best/internal/archivetransfer"
	"felis.lolicon.best/internal/distributed"
	corev1 "k8s.io/api/core/v1"
)

type fakeDistribution struct {
	calls         int
	target, owner string

	// Failover: nodes replaces the default node list; targets are the extra names
	// ValidateNode accepts; lostErr is LostNode's answer; placed lists each node's servers;
	// begun records BeginFailover, beginErr fails it per server.
	nodes        []distributed.Node
	targets      map[string]bool
	lostErr      error
	placed       map[string][]distributed.PlacedServer
	begun        []fakeFailover
	beginErr     map[string]error
	pending      []string
	lost         map[string]time.Time
	controllerUp bool
	failTarget   string
	replicas     []string
}

type fakeFailover struct {
	server, target, owner string
	backup                archivetransfer.Receipt
}

func (d *fakeDistribution) Nodes(context.Context) ([]distributed.Node, error) {
	d.calls++
	if d.nodes != nil {
		return d.nodes, nil
	}
	return []distributed.Node{{Name: "b", Ready: true, Approved: true, Addresses: []string{}}}, nil
}
func (d *fakeDistribution) ValidateNode(_ context.Context, name string) error {
	if name != "b" && !d.targets[name] {
		return errors.New("not approved")
	}
	return nil
}
func (d *fakeDistribution) BeginMigration(_ context.Context, name, target, owner string) (distributed.Operation, error) {
	d.calls++
	d.target = target
	d.owner = owner
	return distributed.Operation{ID: "op", Server: name, State: "backing_up"}, nil
}
func (d *fakeDistribution) Migration(context.Context, string, string) (distributed.Operation, error) {
	d.calls++
	return distributed.Operation{ID: "op", State: "failed"}, nil
}
func (d *fakeDistribution) RetryMigration(context.Context, string, string) (distributed.Operation, error) {
	d.calls++
	return distributed.Operation{ID: "op", State: "restoring"}, nil
}
func (d *fakeDistribution) AbortMigration(context.Context, string, string) (distributed.Operation, error) {
	d.calls++
	return distributed.Operation{ID: "op", State: "aborted"}, nil
}

func TestDistributedRoutesAreAdministratorOnly(t *testing.T) {
	for _, role := range []string{"user", "admin"} {
		t.Run(role, func(t *testing.T) {
			repo := newFakeRepo()
			repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "owner"}
			a := newTestAPI(repo, newFakeCluster())
			a.External = staticExternal{p: &Principal{UserID: "owner", Role: role, ViaAdminAccess: role == "admin"}}
			d := &fakeDistribution{}
			a.Distribution = d
			for _, tc := range []struct {
				method, path, body string
				status             int
			}{
				{"GET", "/api/v1/nodes", "", 200},
				{"POST", "/api/v1/servers/survival/migrations", `{"targetNode":"b"}`, 202},
				{"GET", "/api/v1/servers/survival/migrations", "", 200},
				{"GET", "/api/v1/servers/survival/migrations/op", "", 200},
				{"POST", "/api/v1/servers/survival/migrations/op/retry", "", 202},
				{"POST", "/api/v1/servers/survival/migrations/op/abort", "", 200},
			} {
				w := do(a.ExternalHandler(), tc.method, tc.path, tc.body, jsonHeader)
				want := tc.status
				if role == "user" {
					want = 403
				}
				if w.Code != want {
					t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
				}
			}
			if role == "user" && d.calls != 0 {
				t.Fatal("owner reached cluster-wide operations")
			}
			if role == "admin" && (d.target != "b" || d.owner != "owner") {
				t.Fatal("wrong migration scope")
			}
		})
	}
}
func (d *fakeDistribution) LostNode(_ context.Context, name string) (*corev1.Node, error) {
	if d.lostErr != nil {
		return nil, d.lostErr
	}
	return &corev1.Node{}, nil
}
func (d *fakeDistribution) ServersOn(_ context.Context, node string) ([]distributed.PlacedServer, error) {
	return d.placed[node], nil
}
func (d *fakeDistribution) BeginFailover(_ context.Context, server, target, owner string, backup archivetransfer.Receipt) (distributed.Operation, error) {
	if err := d.beginErr[server]; err != nil {
		return distributed.Operation{}, err
	}
	d.begun = append(d.begun, fakeFailover{server, target, owner, backup})
	return distributed.Operation{ID: "fo-" + server, Kind: distributed.KindFailover, Server: server, TargetNode: target, State: "fencing"}, nil
}
func (d *fakeDistribution) FencePending(context.Context) ([]string, error) { return d.pending, nil }
func (d *fakeDistribution) FailoverTarget(context.Context) (string, error) {
	if d.failTarget == "" {
		return "", errors.New("no online node can take the worlds")
	}
	return d.failTarget, nil
}
func (d *fakeDistribution) LostNodes(context.Context) (map[string]time.Time, bool, error) {
	return d.lost, d.controllerUp, nil
}
func (d *fakeDistribution) ReplicaCandidates(context.Context) ([]string, error) {
	return d.replicas, nil
}
