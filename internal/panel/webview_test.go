package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsInAppWebView(t *testing.T) {
	cases := []struct {
		name string
		ua   string
		want bool
	}{
		{"wechat", "Mozilla/5.0 (iPhone; CPU iPhone OS 16_0) AppleWebKit/605 MicroMessenger/8.0.30(0x18001e2f) NetType/WIFI", true},
		{"wechat-android", "Mozilla/5.0 (Linux; Android 13) MicroMessenger/8.0.40 Mobile", true},
		{"qq-inapp", "Mozilla/5.0 (iPhone; CPU iPhone OS 16_0) AppleWebKit/605 QQ/8.9.68 V1_IPH", true},
		{"qq-mqqbrowser", "Mozilla/5.0 (Linux; Android 12) MQQBrowser/13.6 Mobile Safari/537.36", true},
		{"plain-chrome", "Mozilla/5.0 (Windows NT 10.0) AppleWebKit/537.36 Chrome/120 Safari/537.36", false},
		{"mobile-safari", "Mozilla/5.0 (iPhone; CPU iPhone OS 16_0) AppleWebKit/605 Version/16 Mobile Safari/604", false},
		{"empty", "", false},
		// "QQ" only counts as the chat-webview token " QQ/"; a bare substring must not
		// trip the guard (avoid false positives on unrelated agents).
		{"qqbrowser-standalone", "Mozilla/5.0 (Linux; Android 12) QQBrowser/13.6", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isInAppWebView(c.ua); got != c.want {
				t.Fatalf("isInAppWebView(%q) = %v, want %v", c.ua, got, c.want)
			}
		})
	}
}

// newHandler builds a panel handler with an api stub that fails the test if the guard
// ever leaks a WebView navigation through to it.
func newPanelHandler(t *testing.T) http.Handler {
	t.Helper()
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	return Handler(api, "example.test", "", "", 0, "", "")
}

func TestGuardServesInterstitialForWeChatNavigation(t *testing.T) {
	h := newPanelHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/onboarding", nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("User-Agent", "MicroMessenger/8.0.30")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	// It must be the interstitial, NOT the SPA index (which contains "Felis").
	if !strings.Contains(body, "系统浏览器") || !strings.Contains(body, "Passkey") {
		t.Fatalf("body is not the interstitial: %q", body)
	}
	if strings.Contains(w.Header().Get("Content-Type"), "text/html") == false {
		t.Fatalf("content-type = %q", w.Header().Get("Content-Type"))
	}
	// The absolute URL to copy must reflect the request host+path (https default).
	if !strings.Contains(body, "https://example.com/onboarding") && !strings.Contains(body, "example.com/onboarding") {
		// httptest default host is example.com
		t.Fatalf("interstitial missing external URL, body: %q", body)
	}
}

func TestGuardReflectedURLIsEscaped(t *testing.T) {
	h := newPanelHandler(t)
	// A crafted path/query must be HTML-escaped in the reflected URL, not injected raw.
	req := httptest.NewRequest(http.MethodGet, "/x?q=%22%3E%3Cscript%3Ealert(1)%3C/script%3E", nil)
	req.Header.Set("Accept", "text/html")
	req.Header.Set("User-Agent", "MicroMessenger")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if strings.Contains(w.Body.String(), "<script>alert(1)</script>") {
		t.Fatalf("reflected URL was not escaped: %q", w.Body.String())
	}
}

func TestGuardPassesThroughNonWebView(t *testing.T) {
	h := newPanelHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept", "text/html")
	req.Header.Set("User-Agent", "Mozilla/5.0 Chrome/120 Safari/537.36")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Felis") {
		t.Fatalf("normal browser did not get the SPA: %d %q", w.Code, w.Body.String())
	}
}

func TestGuardIgnoresAssetAndAPIRequests(t *testing.T) {
	h := newPanelHandler(t)

	// An asset request from the SAME WebView (no Accept: text/html) must pass through,
	// so an acknowledged SPA can still load its scripts.
	req := httptest.NewRequest(http.MethodGet, "/config.json", nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "MicroMessenger")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "系统浏览器") {
		t.Fatalf("config.json was intercepted: %d", w.Code)
	}

	// An API call from a WebView must reach the api handler, not the interstitial.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("Accept", "text/html") // even if it claims html
	req.Header.Set("User-Agent", "MicroMessenger")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusTeapot {
		t.Fatalf("api call intercepted: %d", w.Code)
	}
}

func TestGuardHonorsAcknowledgement(t *testing.T) {
	h := newPanelHandler(t)

	// The ua_ack escape hatch: it must plant the ack cookie AND serve the SPA.
	req := httptest.NewRequest(http.MethodGet, "/onboarding?ua_ack=1", nil)
	req.Header.Set("Accept", "text/html")
	req.Header.Set("User-Agent", "MicroMessenger")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Felis") {
		t.Fatalf("ua_ack did not serve the SPA: %d %q", w.Code, w.Body.String())
	}
	var acked bool
	for _, c := range w.Result().Cookies() {
		if c.Name == webViewAckCookie && c.Value == "1" && c.Secure {
			acked = true
		}
	}
	if !acked {
		t.Fatalf("ua_ack did not set a Secure ack cookie")
	}

	// A subsequent navigation carrying the ack cookie is not interrupted.
	req = httptest.NewRequest(http.MethodGet, "/onboarding", nil)
	req.Header.Set("Accept", "text/html")
	req.Header.Set("User-Agent", "MicroMessenger")
	req.AddCookie(&http.Cookie{Name: webViewAckCookie, Value: "1"})
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Felis") {
		t.Fatalf("ack cookie was not honored: %d %q", w.Code, w.Body.String())
	}
}
