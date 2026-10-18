package registryprune

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"felis.lolicon.best/internal/registrygate"
)

// Client reads registry metadata anonymously and deletes as the prune principal.
type Client struct {
	Endpoint string
	Token    string
	// Nil HTTP uses a 30s timeout.
	HTTP *http.Client
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (c *Client) Repositories(ctx context.Context) ([]string, error) {
	var out []string
	next := c.Endpoint + "/v2/_catalog?n=1000"
	for next != "" {
		var page struct {
			Repositories []string `json:"repositories"`
		}
		resp, err := c.get(ctx, next, &page)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Repositories...)
		next = ""
		if link := resp.Header.Get("Link"); strings.Contains(link, `rel="next"`) {
			start, end := strings.Index(link, "<"), strings.Index(link, ">")
			if start < 0 || end <= start {
				return nil, fmt.Errorf("malformed catalog Link header %q", link)
			}
			u, err := url.Parse(c.Endpoint)
			if err != nil {
				return nil, err
			}
			ref, err := url.Parse(link[start+1 : end])
			if err != nil {
				return nil, err
			}
			next = u.ResolveReference(ref).String()
		}
	}
	return out, nil
}

func (c *Client) Index(ctx context.Context, repo string) (*registrygate.Index, error) {
	var idx registrygate.Index
	if _, err := c.get(ctx, c.Endpoint+registrygate.IndexPathPrefix+repo, &idx); err != nil {
		return nil, err
	}
	return &idx, nil
}

// Delete succeeds if the manifest is already absent.
func (c *Client) Delete(ctx context.Context, repo, digest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.Endpoint+"/v2/"+repo+"/manifests/"+digest, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(registrygate.PrincipalPrune, c.Token)
	resp, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusAccepted, http.StatusOK, http.StatusNotFound:
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return fmt.Errorf("registry answered %s: %s", resp.Status, strings.TrimSpace(string(b)))
}

func (c *Client) get(ctx context.Context, u string, into any) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("GET %s: registry answered %s: %s", u, resp.Status, strings.TrimSpace(string(b)))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(into); err != nil {
		return nil, fmt.Errorf("GET %s: %w", u, err)
	}
	return resp, nil
}
