package api

import (
	"context"
	"errors"
	"testing"

	"felis.lolicon.best/internal/nodecontrol"
)

type fakeNodeControl struct {
	tasks   []nodecontrol.Task
	calls   int
	err     error
	request nodecontrol.Request
	actor   string
}

func (f *fakeNodeControl) List(context.Context) ([]nodecontrol.Task, error) { return f.tasks, f.err }
func (f *fakeNodeControl) Get(context.Context, string) (nodecontrol.Task, error) {
	if f.err != nil {
		return nodecontrol.Task{}, f.err
	}
	if len(f.tasks) == 0 {
		return nodecontrol.Task{}, nodecontrol.ErrNotFound
	}
	return f.tasks[0], nil
}
func (f *fakeNodeControl) Start(_ context.Context, r nodecontrol.Request, actor string) (nodecontrol.Task, error) {
	f.calls++
	f.request = r
	f.actor = actor
	return nodecontrol.Task{ID: "task", Request: r, State: "running"}, f.err
}

const nodeTaskPath = "/api/v1/settings/node-control/tasks"
const nodeTaskBody = `{"action":"approve","name":"worker-01","sshTarget":"worker-01","confirmMaintenance":true}`

func TestNodeControlOwnerBoundaryAndRetries(t *testing.T) {
	for _, role := range []string{"user", "admin", "owner"} {
		a := newTestAPI(newFakeRepo(), newFakeCluster())
		a.External = staticExternal{p: &Principal{UserID: "actor", Role: role, ViaAdminAccess: role != "user"}}
		control := &fakeNodeControl{tasks: []nodecontrol.Task{{ID: "task", State: "failed", Request: nodecontrol.Request{Action: "approve", Name: "worker-01", SSHTarget: "worker-01", ConfirmMaintenance: true}}}}
		a.NodeControl = control
		for _, tc := range []struct {
			method, path, body string
			want               int
		}{{"GET", "/api/v1/settings/node-control", "", 200}, {"GET", nodeTaskPath + "/task", "", 200}, {"POST", nodeTaskPath, nodeTaskBody, 202}, {"POST", nodeTaskPath + "/task/retry", "", 202}} {
			w := do(a.ExternalHandler(), tc.method, tc.path, tc.body, jsonHeader)
			want := tc.want
			if role != "owner" {
				want = 403
			}
			if w.Code != want {
				t.Fatalf("%s %s: %d %s", role, tc.path, w.Code, w.Body.String())
			}
		}
		if role != "owner" && control.calls != 0 {
			t.Fatal("nonowner reached root operations")
		}
		if role == "owner" && (control.actor != "actor" || control.request.Name != "worker-01") {
			t.Fatal("actor/scope not preserved")
		}
	}
}
func TestNodeControlErrorsAndStartGuard(t *testing.T) {
	a := newTestAPI(newFakeRepo(), newFakeCluster())
	a.External = staticExternal{p: &Principal{UserID: "owner", Role: "owner", ViaAdminAccess: true}}
	control := &fakeNodeControl{}
	a.NodeControl = control
	w := do(a.ExternalHandler(), "POST", nodeTaskPath, `{"action":"approve","name":"x","sshTarget":"-exec","confirmMaintenance":true}`, jsonHeader)
	if w.Code != 400 || control.calls != 0 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, tc := range []struct {
		err  error
		code int
	}{{nodecontrol.ErrBusy, 409}, {errors.New("private host detail"), 503}} {
		control.err = tc.err
		w := do(a.ExternalHandler(), "POST", nodeTaskPath, nodeTaskBody, jsonHeader)
		if w.Code != tc.code {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	guard := NodeMaintenanceGuard(control)
	if guard(context.Background()) == nil {
		t.Fatal("host failure permitted start")
	}
	control.err = nil
	control.tasks = []nodecontrol.Task{{State: "running"}}
	if guard(context.Background()) == nil {
		t.Fatal("maintenance permitted start")
	}
	control.tasks[0].State = "failed"
	if err := guard(context.Background()); err != nil {
		t.Fatal(err)
	}
	control.tasks[0].State = "succeeded"
	w = do(a.ExternalHandler(), "POST", nodeTaskPath+"/task/retry", "", jsonHeader)
	if w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestNodeControlRequiresFreshOwnerProof(t *testing.T) {
	repo := newFakeRepo()
	a := newTestAPI(repo, newFakeCluster())
	p := &Principal{UserID: "owner", Role: "owner", ViaAdminAccess: true, ViaSession: true}
	repo.passkeyCreds["owner-key"] = PasskeyCredential{ID: "owner-key", UserID: "owner", UserVerified: true}
	a.External = staticExternal{p: p}
	control := &fakeNodeControl{}
	a.NodeControl = control
	w := do(a.ExternalHandler(), "POST", nodeTaskPath, nodeTaskBody, jsonHeader)
	if w.Code != 403 || decodeErr(t, w) != "reauth_required" || control.calls != 0 {
		t.Fatal(w.Code, w.Body.String())
	}
	p.ReauthAt = a.now()
	w = do(a.ExternalHandler(), "POST", nodeTaskPath, nodeTaskBody, jsonHeader)
	if w.Code != 202 || control.calls != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
}
