package cfsetup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// This file is INTEGRATION-ONLY. ExecRunner shells out to the real `cloudflared`
// binary and calls the live Cloudflare API; none of it can run — or be honestly
// faked — on a box without the operator's own Cloudflare account and the
// interactive `cloudflared tunnel login` consent already completed. The orchestration
// that uses it (Setup) and the request-body/guard logic are unit-verified in
// cfsetup.go; what lives here is exercised only against a real account.

const defaultAPIBase = "https://api.cloudflare.com/client/v4"

// DetectPreconditions inspects the local environment for the gating facts Setup
// needs: whether cloudflared is installed and whether the operator has logged in
// (cert.pem present). The API token is supplied by the caller (the TUI prompts
// for it); it is passed through so the returned value is ready to hand to Setup.
// This only READS the environment — it performs no Cloudflare side effects — but
// it touches the real filesystem/PATH, so it is integration-side.
func DetectPreconditions(apiToken string) Preconditions {
	pre := Preconditions{APIToken: apiToken}
	if path, err := exec.LookPath("cloudflared"); err == nil {
		pre.CloudflaredPath = path
	}
	if home, err := os.UserHomeDir(); err == nil {
		if _, err := os.Stat(filepath.Join(home, ".cloudflared", "cert.pem")); err == nil {
			pre.CertExists = true
		}
	}
	return pre
}

// ExecRunner is the production Runner: cloudflared via os/exec for the tunnel and
// the Cloudflare API via HTTP for Access.
type ExecRunner struct {
	// Cloudflared is the resolved cloudflared binary path (Preconditions.CloudflaredPath).
	Cloudflared string
	// APIToken authenticates the Access API calls (Bearer).
	APIToken string
	// AccountID is the Cloudflare account the Access app/policy are created under.
	AccountID string
	// APIBase defaults to the public Cloudflare API; overridable for testing.
	APIBase string
	// HTTP is the client used for API calls; nil means a default with a timeout.
	HTTP *http.Client
}

func (r *ExecRunner) httpClient() *http.Client {
	if r.HTTP != nil {
		return r.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (r *ExecRunner) apiBase() string {
	if r.APIBase != "" {
		return r.APIBase
	}
	return defaultAPIBase
}

// tunnelIDRE extracts the UUID cloudflared prints when a tunnel is created or
// already exists.
var tunnelIDRE = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// CreateTunnel runs `cloudflared tunnel create <name>`. cloudflared writes the
// credentials JSON under ~/.cloudflared/<id>.json and prints the id; we parse it
// out. If the tunnel already exists this returns its id (idempotent re-run).
func (r *ExecRunner) CreateTunnel(ctx context.Context, name string) (string, string, error) {
	out, err := r.runCloudflared(ctx, "tunnel", "create", name)
	if err != nil {
		// An "already exists" is not fatal — recover the id via `tunnel list`.
		if id, lerr := r.lookupTunnel(ctx, name); lerr == nil && id != "" {
			return id, r.credentialsPath(id), nil
		}
		return "", "", err
	}
	id := tunnelIDRE.FindString(out)
	if id == "" {
		return "", "", fmt.Errorf("cfsetup: could not parse tunnel id from cloudflared output: %s", out)
	}
	return id, r.credentialsPath(id), nil
}

// lookupTunnel finds an existing tunnel's id by name via `tunnel list`.
func (r *ExecRunner) lookupTunnel(ctx context.Context, name string) (string, error) {
	out, err := r.runCloudflared(ctx, "tunnel", "list", "--name", name, "--output", "json")
	if err != nil {
		return "", err
	}
	var tunnels []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(out), &tunnels); err != nil {
		return "", err
	}
	for _, t := range tunnels {
		if t.Name == name {
			return t.ID, nil
		}
	}
	return "", fmt.Errorf("cfsetup: tunnel %q not found", name)
}

func (r *ExecRunner) credentialsPath(id string) string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".cloudflared", id+".json")
	}
	return id + ".json"
}

// RouteDNS runs `cloudflared tunnel route dns <tunnelID> <hostname>`, creating the
// proxied CNAME. It is idempotent on cloudflared's side for an existing record.
func (r *ExecRunner) RouteDNS(ctx context.Context, tunnelID, hostname string) error {
	_, err := r.runCloudflared(ctx, "tunnel", "route", "dns", tunnelID, hostname)
	if err != nil && strings.Contains(err.Error(), "already exists") {
		return nil
	}
	return err
}

// WriteTunnelConfig writes the rendered config.yml, creating its parent directory.
func (r *ExecRunner) WriteTunnelConfig(path string, contents []byte) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, contents, 0o644)
}

// CreateAccessApplication POSTs the self-hosted Access app and returns its id and
// issued aud (spec §14: the aud felis [auth] access_jwt_aud must adopt).
func (r *ExecRunner) CreateAccessApplication(ctx context.Context, app AccessApplication) (string, string, error) {
	var resp struct {
		Result struct {
			ID  string `json:"id"`
			AUD string `json:"aud"`
		} `json:"result"`
	}
	if err := r.apiPost(ctx, fmt.Sprintf("/accounts/%s/access/apps", r.AccountID), app, &resp); err != nil {
		// If application already exists, look it up instead of failing (idempotency)
		if strings.Contains(err.Error(), "application_already_exists") || strings.Contains(err.Error(), "11010") {
			if id, aud, lerr := r.lookupAccessApplication(ctx, app.Domain); lerr == nil && id != "" {
				return id, aud, nil
			}
		}
		return "", "", err
	}
	return resp.Result.ID, resp.Result.AUD, nil
}

// CreateAccessPolicy POSTs the policy onto the Access app.
func (r *ExecRunner) CreateAccessPolicy(ctx context.Context, appID string, policy AccessPolicy) error {
	if err := r.apiPost(ctx, fmt.Sprintf("/accounts/%s/access/apps/%s/policies", r.AccountID, appID), policy, nil); err != nil {
		// If policy already exists, treat it as idempotent success
		if strings.Contains(err.Error(), "policy_already_exists") || strings.Contains(err.Error(), "11015") || strings.Contains(err.Error(), "already_exists") {
			return nil
		}
		return err
	}
	return nil
}

// runCloudflared executes the cloudflared binary with the given args, returning
// combined output. The interactive `tunnel login` browser consent is NOT done
// here — it is a separate, operator-driven step the TUI suspends to run.
func (r *ExecRunner) runCloudflared(ctx context.Context, args ...string) (string, error) {
	bin := r.Cloudflared
	if bin == "" {
		bin = "cloudflared"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return buf.String(), fmt.Errorf("cfsetup: cloudflared %s: %w: %s", strings.Join(args, " "), err, buf.String())
	}
	return buf.String(), nil
}

// apiPost sends an authenticated JSON POST to the Cloudflare API and, on a
// non-2xx or success:false body, returns the error. out, when non-nil, receives
// the decoded response.
func (r *ExecRunner) apiPost(ctx context.Context, path string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.apiBase()+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.APIToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("cfsetup: Cloudflare API authentication failed (status 401). Please verify that:\n"+
				"  1. The API Token is valid, active, and has not expired.\n"+
				"  2. You did not enter a Global API Key (a Bearer API Token is required).\n"+
				"  3. The token has the required permissions under the Account scope:\n"+
				"     - Account > Access Apps and Policies: Edit\n"+
				"     - Account > Cloudflare Tunnel: Edit\n"+
				"     - Zone > DNS: Edit\n"+
				"  Original error: %s", string(raw))
		}
		if resp.StatusCode == http.StatusForbidden {
			return fmt.Errorf("cfsetup: Cloudflare API access forbidden (status 403). Please verify that:\n"+
				"  1. The API Token has permission to access Account ID %q.\n"+
				"  2. The token has the required permissions under the Account scope:\n"+
				"     - Account > Access Apps and Policies: Edit\n"+
				"     - Account > Cloudflare Tunnel: Edit\n"+
				"     - Zone > DNS: Edit\n"+
				"  Original error: %s", r.AccountID, string(raw))
		}
		return fmt.Errorf("cfsetup: Cloudflare API %s: status %d: %s", path, resp.StatusCode, string(raw))
	}
	// Cloudflare wraps every response in {success, errors, result}; surface a
	// success:false even on a 200.
	var envelope struct {
		Success bool              `json:"success"`
		Errors  []json.RawMessage `json:"errors"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && !envelope.Success && len(envelope.Errors) > 0 {
		return fmt.Errorf("cfsetup: Cloudflare API %s: %s", path, string(raw))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("cfsetup: decode Cloudflare API %s response: %w", path, err)
		}
	}
	return nil
}

// apiGet sends an authenticated JSON GET to the Cloudflare API and, on a
// non-2xx or success:false body, returns the error. out, when non-nil, receives
// the decoded response.
func (r *ExecRunner) apiGet(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.apiBase()+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.APIToken)
	resp, err := r.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("cfsetup: Cloudflare API authentication failed (status 401). Please verify that:\n"+
				"  1. The API Token is valid, active, and has not expired.\n"+
				"  2. You did not enter a Global API Key (a Bearer API Token is required).\n"+
				"  3. The token has the required permissions under the Account scope:\n"+
				"     - Account > Access Apps and Policies: Edit\n"+
				"     - Account > Cloudflare Tunnel: Edit\n"+
				"     - Zone > DNS: Edit\n"+
				"  Original error: %s", string(raw))
		}
		if resp.StatusCode == http.StatusForbidden {
			return fmt.Errorf("cfsetup: Cloudflare API access forbidden (status 403). Please verify that:\n"+
				"  1. The API Token has permission to access Account ID %q.\n"+
				"  2. The token has the required permissions under the Account scope:\n"+
				"     - Account > Access Apps and Policies: Edit\n"+
				"     - Account > Cloudflare Tunnel: Edit\n"+
				"     - Zone > DNS: Edit\n"+
				"  Original error: %s", r.AccountID, string(raw))
		}
		return fmt.Errorf("cfsetup: Cloudflare API %s: status %d: %s", path, resp.StatusCode, string(raw))
	}
	var envelope struct {
		Success bool              `json:"success"`
		Errors  []json.RawMessage `json:"errors"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && !envelope.Success && len(envelope.Errors) > 0 {
		return fmt.Errorf("cfsetup: Cloudflare API %s: %s", path, string(raw))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("cfsetup: decode Cloudflare API %s response: %w", path, err)
		}
	}
	return nil
}

// lookupAccessApplication finds an existing Access application's id and aud by domain.
func (r *ExecRunner) lookupAccessApplication(ctx context.Context, domain string) (string, string, error) {
	var resp struct {
		Result []struct {
			ID     string `json:"id"`
			Domain string `json:"domain"`
			AUD    string `json:"aud"`
		} `json:"result"`
	}
	if err := r.apiGet(ctx, fmt.Sprintf("/accounts/%s/access/apps?per_page=100", r.AccountID), &resp); err != nil {
		return "", "", err
	}
	for _, app := range resp.Result {
		if app.Domain == domain {
			return app.ID, app.AUD, nil
		}
	}
	return "", "", fmt.Errorf("cfsetup: access application for domain %q not found in list", domain)
}
