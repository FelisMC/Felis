package api

import (
	"context"
	"errors"
	"testing"

	"felis.lolicon.best/internal/distributed"
)

type fakeDistribution struct {
	calls         int
	target, owner string
}

func (d *fakeDistribution) Nodes(context.Context) ([]distributed.Node, error) {
	d.calls++
	return []distributed.Node{{Name: "b", Ready: true, Approved: true, Addresses: []string{}}}, nil
}
func (d *fakeDistribution) ValidateNode(_ context.Context, name string) error {
	if name != "b" {
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
