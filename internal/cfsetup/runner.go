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

// Integration-only: needs the real cloudflared binary and a Cloudflare account.

const defaultAPIBase = "https://api.cloudflare.com/client/v4"

// Reads the environment only (cloudflared on PATH, cert.pem); no Cloudflare side effects.
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

// ExecRunner invokes cloudflared for tunnels and the Cloudflare HTTP API for Access.
type ExecRunner struct {
	Cloudflared string
	APIToken    string
	AccountID   string
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

// Read-only check of the token against the account and permission Setup writes to; fails before any tunnel or DNS change.
func (r *ExecRunner) VerifyAPIToken(ctx context.Context) error {
	if strings.TrimSpace(r.APIToken) == "" {
		return fmt.Errorf("cfsetup: Cloudflare API token is required")
	}
	if strings.TrimSpace(r.AccountID) == "" {
		return fmt.Errorf("cfsetup: Cloudflare account ID is required")
	}
	return r.apiGet(ctx, fmt.Sprintf("/accounts/%s/access/apps?per_page=1", r.AccountID), nil)
}

var tunnelIDRE = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// Runs cloudflared tunnel create; an existing tunnel returns its id, so re-runs are safe.
func (r *ExecRunner) CreateTunnel(ctx context.Context, name string) (string, string, error) {
	var id string
	out, err := r.runCloudflared(ctx, "tunnel", "create", name)
	if err != nil {
		lid, lerr := r.lookupTunnel(ctx, name)
		if lerr != nil || lid == "" {
			return "", "", err
		}
		id = lid
	} else if id = tunnelIDRE.FindString(out); id == "" {
		return "", "", fmt.Errorf("cfsetup: could not parse tunnel id from cloudflared output: %s", out)
	}
	cred := r.credentialsPath(id)
	if err := r.ensureCredentials(ctx, id, cred); err != nil {
		return "", "", err
	}
	return id, cred, nil
}

// Ensures the credentials file exists; a re-run without it re-fetches the token with cert.pem, keeping the tunnel id.
func (r *ExecRunner) ensureCredentials(ctx context.Context, id, credPath string) error {
	state := "missing"
	if _, err := os.Stat(credPath); err == nil {
		problem := credentialsProblem(credPath, id)
		if problem == "" {
			return nil
		}
		if err := os.Rename(credPath, credPath+".invalid"); err != nil {
			return fmt.Errorf("cfsetup: tunnel %s credentials file %s %s, and moving it aside failed: %w", id, credPath, problem, err)
		}
		state = problem + " (kept as " + credPath + ".invalid)"
	}
	if err := os.MkdirAll(filepath.Dir(credPath), 0o700); err != nil {
		return fmt.Errorf("cfsetup: preparing credentials dir for tunnel %s: %w", id, err)
	}
	if _, err := r.runCloudflared(ctx, "tunnel", "token", "--cred-file", credPath, id); err != nil {
		return fmt.Errorf("cfsetup: tunnel %s credentials file %s is %s and could not be regenerated: %w", id, credPath, state, err)
	}
	if problem := credentialsProblem(credPath, id); problem != "" {
		return fmt.Errorf("cfsetup: cloudflared regenerated tunnel %s credentials at %s, but the file %s", id, credPath, problem)
	}
	// Tunnel credentials must be owner-readable only, regardless of cloudflared's umask.
	if err := os.Chmod(credPath, 0o600); err != nil {
		return fmt.Errorf("cfsetup: securing credentials file %s: %w", credPath, err)
	}
	return nil
}

// Returns why the credentials file cannot run the tunnel, or "" when it can.
func credentialsProblem(path, id string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "cannot be read (" + err.Error() + ")"
	}
	var cred struct {
		AccountTag   string `json:"AccountTag"`
		TunnelSecret string `json:"TunnelSecret"`
		TunnelID     string `json:"TunnelID"`
	}
	if err := json.Unmarshal(raw, &cred); err != nil {
		return "is not valid JSON"
	}
	switch {
	case cred.AccountTag == "" || cred.TunnelSecret == "" || cred.TunnelID == "":
		return "lacks AccountTag, TunnelSecret or TunnelID"
	case !strings.EqualFold(cred.TunnelID, id):
		return "belongs to tunnel " + cred.TunnelID
	}
	return ""
}

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

// --overwrite-dns is required: without it a stale CNAME from a deleted tunnel makes cloudflared refuse, and the name stays on the dead tunnel.
func (r *ExecRunner) RouteDNS(ctx context.Context, tunnelID, hostname string) error {
	_, err := r.runCloudflared(ctx, "tunnel", "route", "dns", "--overwrite-dns", tunnelID, hostname)
	return err
}

func (r *ExecRunner) WriteTunnelConfig(path string, contents []byte) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, contents, 0o644)
}

// Creates the self-hosted Access app and returns its id and aud (the aud felis [auth] access_jwt_aud must adopt).
func (r *ExecRunner) CreateAccessApplication(ctx context.Context, app AccessApplication) (string, string, error) {
	var resp struct {
		Result struct {
			ID  string `json:"id"`
			AUD string `json:"aud"`
		} `json:"result"`
	}
	if err := r.apiPost(ctx, fmt.Sprintf("/accounts/%s/access/apps", r.AccountID), app, &resp); err != nil {
		if strings.Contains(err.Error(), "application_already_exists") || strings.Contains(err.Error(), "11010") {
			if id, aud, lerr := r.lookupAccessApplication(ctx, app.Domain); lerr == nil && id != "" {
				return id, aud, nil
			}
		}
		return "", "", err
	}
	return resp.Result.ID, resp.Result.AUD, nil
}

// Upserts the fail-closed policy by name, overwriting any policy of that name so no older, broader rule set survives.
func (r *ExecRunner) CreateAccessPolicy(ctx context.Context, appID string, policy AccessPolicy) error {
	pid, err := r.lookupAccessPolicy(ctx, appID, policy.Name)
	if err != nil {
		return err
	}
	if pid != "" {
		return r.apiPut(ctx, fmt.Sprintf("/accounts/%s/access/apps/%s/policies/%s", r.AccountID, appID, pid), policy, nil)
	}
	if err := r.apiPost(ctx, fmt.Sprintf("/accounts/%s/access/apps/%s/policies", r.AccountID, appID), policy, nil); err != nil {
		if strings.Contains(err.Error(), "already_exists") || strings.Contains(err.Error(), "11015") {
			if pid, lerr := r.lookupAccessPolicy(ctx, appID, policy.Name); lerr == nil && pid != "" {
				return r.apiPut(ctx, fmt.Sprintf("/accounts/%s/access/apps/%s/policies/%s", r.AccountID, appID, pid), policy, nil)
			}
		}
		return err
	}
	return nil
}

func (r *ExecRunner) lookupAccessPolicy(ctx context.Context, appID, name string) (string, error) {
	var resp struct {
		Result []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"result"`
	}
	if err := r.apiGet(ctx, fmt.Sprintf("/accounts/%s/access/apps/%s/policies?per_page=100", r.AccountID, appID), &resp); err != nil {
		return "", err
	}
	for _, p := range resp.Result {
		if p.Name == name {
			return p.ID, nil
		}
	}
	return "", nil
}

// Runs cloudflared and returns combined output. Interactive tunnel login is handled by the TUI, not here.
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

// Authenticated JSON write to the Cloudflare API; errors on non-2xx or success:false.
func (r *ExecRunner) apiPost(ctx context.Context, path string, body, out any) error {
	return r.apiWrite(ctx, http.MethodPost, path, body, out)
}

func (r *ExecRunner) apiPut(ctx context.Context, path string, body, out any) error {
	return r.apiWrite(ctx, http.MethodPut, path, body, out)
}

func (r *ExecRunner) apiWrite(ctx context.Context, method, path string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, r.apiBase()+path, bytes.NewReader(payload))
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
	// Cloudflare can return success:false with HTTP 200.
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

// Authenticated JSON GET to the Cloudflare API; errors on non-2xx or success:false.
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
