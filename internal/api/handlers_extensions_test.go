package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"felis.lolicon.best/internal/fileedit"
)

const extensionsRoute = "/api/v1/servers/survival/extensions"

func TestInstalledExtensionsListing(t *testing.T) {
	for _, tc := range []struct {
		name      string
		entries   []fileedit.Extension
		truncated bool
		want      string
	}{
		{
			name: "empty directories",
			want: `{"entries":[],"truncated":false}`,
		},
		{
			name: "metadata and damaged jars remain in one listing",
			entries: []fileedit.Extension{
				{
					Path: "plugins/Example.jar", Filename: "Example.jar", Size: 1234,
					Enabled: true, Kind: "plugin", MetadataStatus: "ok", ConfigPath: "plugins/Example",
					Metadata: []fileedit.ExtensionMetadata{{ID: "Example", Name: "Example Plugin", Version: "1.2.3", Authors: []string{"Example Author"}, Loaders: []string{"paper"}}},
				},
				{
					Path: "mods/multiple.jar.disabled", Filename: "multiple.jar.disabled", Size: 2345,
					Kind: "mod", MetadataStatus: "ok",
					Metadata: []fileedit.ExtensionMetadata{
						{ID: "first", Name: "First Mod", Version: "2.0", Authors: []string{"First Author"}, Loaders: []string{"forge", "neoforge"}},
						{ID: "second", Name: "Second Mod"},
					},
				},
				{Path: "mods/broken.jar", Filename: "broken.jar", Size: 5, Enabled: true, Kind: "mod", MetadataStatus: "unavailable"},
			},
			truncated: true,
			want: `{"entries":[
				{"path":"plugins/Example.jar","filename":"Example.jar","size":1234,"enabled":true,"kind":"plugin","metadata_status":"ok","metadata":[{"id":"Example","name":"Example Plugin","version":"1.2.3","authors":["Example Author"],"loaders":["paper"]}],"config_path":"plugins/Example"},
				{"path":"mods/multiple.jar.disabled","filename":"multiple.jar.disabled","size":2345,"enabled":false,"kind":"mod","metadata_status":"ok","metadata":[{"id":"first","name":"First Mod","version":"2.0","authors":["First Author"],"loaders":["forge","neoforge"]},{"id":"second","name":"Second Mod","authors":[],"loaders":[]}]},
				{"path":"mods/broken.jar","filename":"broken.jar","size":5,"enabled":true,"kind":"mod","metadata_status":"unavailable","metadata":[]}
			],"truncated":true}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, repo, cl, files := mkFiles(t)
			a.External = staticExternal{p: fileOwner}
			files.extensions, files.truncated = tc.entries, tc.truncated
			w := do(a.ExternalHandler(), "GET", extensionsRoute, "", nil)
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			var want map[string]any
			if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
				t.Fatal(err)
			}
			if got := fileAnswer(t, w); !reflect.DeepEqual(got, want) {
				t.Fatalf("listing=%v, want %v", got, want)
			}
			if files.calls != 1 || files.gotOp != "extensions" || files.gotServer != "survival" {
				t.Fatalf("executor calls=%d operation=%q server=%q", files.calls, files.gotOp, files.gotServer)
			}
			if len(cl.acquired) != 0 || len(cl.released) != 0 || len(repo.audits) != 0 {
				t.Fatalf("listing mutated the server: acquired=%v released=%v audits=%v", cl.acquired, cl.released, repo.audits)
			}
		})
	}
}

func TestInstalledExtensionsRequireStableState(t *testing.T) {
	for _, tc := range []struct {
		name, phase, desired string
		ready                bool
	}{
		{"wake requested", "Stopped", "Running", false},
		{"stop requested", "Running", "Stopped", true},
		{"stopped with a remaining pod", "Stopped", "Stopped", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, cl, files := mkFiles(t)
			a.External = staticExternal{p: fileOwner}
			info := cl.byName["survival"]
			info.Phase, info.DesiredState, info.Ready = tc.phase, tc.desired, tc.ready
			w := do(a.ExternalHandler(), "GET", extensionsRoute, "", nil)
			if w.Code != http.StatusConflict || decodeErr(t, w) != "server_transitioning" || files.calls != 0 {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, files.calls, w.Body.String())
			}
		})
	}
}

func TestInstalledExtensionsExecutorErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"read timed out", fmt.Errorf("waiting: %w", context.DeadlineExceeded), http.StatusGatewayTimeout, "files_timeout"},
		{"damaged response", fileedit.ErrReadDamaged, http.StatusBadGateway, "read_damaged"},
		{"server disappeared", fileedit.ErrNotFound, http.StatusNotFound, "not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, _, files := mkFiles(t)
			a.External = staticExternal{p: fileOwner}
			files.err = tc.err
			w := do(a.ExternalHandler(), "GET", extensionsRoute, "", nil)
			if w.Code != tc.status || decodeErr(t, w) != tc.code {
				t.Fatalf("status=%d body=%s, want %d/%s", w.Code, w.Body.String(), tc.status, tc.code)
			}
		})
	}
}
