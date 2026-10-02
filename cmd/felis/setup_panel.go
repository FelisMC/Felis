package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const defaultPanelNodePort = 30443

type panelAccessResult struct {
	url string
	err error
}

func setupPanelNodePort() int {
	raw := strings.TrimSpace(os.Getenv("FELIS_PANEL_NODEPORT"))
	if raw == "" {
		return defaultPanelNodePort
	}
	port, err := strconv.Atoi(raw)
	if err != nil || port < 30000 || port > 32767 {
		return defaultPanelNodePort
	}
	return port
}

func localPanelURL(rootDomain, adminHostname string) string {
	if ip := rootDomainEmbeddedIP(rootDomain); ip != "" {
		return fmt.Sprintf("https://%s:%d", ip, setupPanelNodePort())
	}
	host := defaultAdminHostname(rootDomain, adminHostname)
	if host == "" {
		return ""
	}
	return fmt.Sprintf("https://%s:%d", host, setupPanelNodePort())
}

func rootDomainEmbeddedIP(rootDomain string) string {
	domain := strings.TrimSpace(strings.TrimSuffix(rootDomain, "."))
	for _, suffix := range []string{".nip.io", ".sslip.io"} {
		base := strings.TrimSuffix(domain, suffix)
		if base == domain {
			continue
		}
		if ip := net.ParseIP(base); ip != nil {
			return ip.String()
		}
	}
	return ""
}

// setupGameAddress is where the operator joins in Minecraft to bind the Owner: the
// IP a nip.io or sslip.io root domain spells out (nothing to resolve), otherwise the
// root domain, with the port when it is not Minecraft's default. The proxy lands
// every fresh connection on the login server whatever name it was dialled by.
func setupGameAddress(rootDomain string, gamePort int) string {
	host := rootDomainEmbeddedIP(rootDomain)
	if host == "" {
		host = strings.TrimSpace(strings.TrimSuffix(rootDomain, "."))
	}
	if host == "" {
		return ""
	}
	if gamePort == 0 || gamePort == 25565 {
		return host
	}
	return net.JoinHostPort(host, strconv.Itoa(gamePort))
}

// gameAddrIsIP reports whether addr, a host or host:port, names its host by IP.
func gameAddrIsIP(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	return net.ParseIP(host) != nil
}

func localPanelOrigin() string {
	return fmt.Sprintf("https://127.0.0.1:%d", setupPanelNodePort())
}

func checkPanelAccess(rootDomain, adminHostname string) panelAccessResult {
	base := localPanelURL(rootDomain, adminHostname)
	if base == "" {
		return panelAccessResult{err: fmt.Errorf("root domain is empty")}
	}
	hostURL, err := url.Parse(base)
	if err != nil {
		return panelAccessResult{url: base, err: err}
	}
	probeBase := fmt.Sprintf("https://127.0.0.1:%d", setupPanelNodePort())
	client := &http.Client{
		Timeout:   8 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec
	}
	for _, target := range []string{probeBase + "/healthz", probeBase + "/", probeBase + "/config.json"} {
		req, err := http.NewRequest(http.MethodGet, target, nil)
		if err != nil {
			return panelAccessResult{url: base, err: err}
		}
		req.Host = hostURL.Hostname()
		resp, err := client.Do(req)
		if err != nil {
			return panelAccessResult{url: base, err: err}
		}
		if resp.Body != nil {
			defer resp.Body.Close()
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return panelAccessResult{url: base, err: fmt.Errorf("%s returned HTTP %d", target, resp.StatusCode)}
		}
		if strings.HasSuffix(target, "/config.json") {
			var cfg struct {
				APIBase    string `json:"apiBase"`
				RootDomain string `json:"rootDomain"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
				return panelAccessResult{url: base, err: fmt.Errorf("decode config.json: %w", err)}
			}
			if cfg.APIBase != "/api/v1" || cfg.RootDomain == "" {
				return panelAccessResult{url: base, err: fmt.Errorf("config.json is incomplete")}
			}
		}
	}
	return panelAccessResult{url: base}
}
