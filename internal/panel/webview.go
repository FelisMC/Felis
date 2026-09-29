package panel

import (
	"html/template"
	"net/http"
	"strings"
)

// In-app webview guard (spec §B onboarding; passkey/WebAuthn is unusable inside the
// WeChat and QQ in-app browsers). A player onboards by opening console.<root_domain>
// — often by scanning the console QR with a phone. If the phone's WeChat or QQ scanner
// opens the link, it loads inside that app's WebView, where a WebAuthn ceremony
// silently fails: the player would hit a dead end at the one step (passkey enrollment)
// the console is built around.
//
// So this guard intercepts the top-level HTML navigation from those WebViews and serves
// an interstitial that steers the player to their SYSTEM browser instead of letting the
// broken SPA load. It lives here in the Go static server — NOT in the panel SPA — so the
// frontend is untouched and every navigation that reaches the console (scanned or
// clicked) is covered at one seam.
//
// It is a guide, not a wall: a "continue anyway" link sets an ack cookie so a determined
// user (or a false-positive UA) is never hard-blocked. Only document navigations are
// touched — API calls, /config.json, health probes and asset requests pass straight
// through, so an acknowledged SPA still loads its scripts normally.

// webViewAckCookie records that the visitor chose to proceed past the interstitial, so
// subsequent full page loads in the same WebView are not interrupted again.
const webViewAckCookie = "felis_ua_ack"

// isInAppWebView reports whether the User-Agent is a WeChat or QQ in-app browser. WeChat
// WebViews carry "MicroMessenger"; QQ's in-app browser carries "MQQBrowser" and the QQ
// chat WebView carries a " QQ/<version>" token. Matched case-insensitively. Standalone
// real browsers (Chrome, Safari, Firefox, even the standalone QQ Browser app) are not
// matched — the target is specifically the chat-app WebViews where passkey breaks.
func isInAppWebView(ua string) bool {
	if ua == "" {
		return false
	}
	l := strings.ToLower(ua)
	return strings.Contains(l, "micromessenger") || // WeChat WebView
		strings.Contains(l, "mqqbrowser") || // QQ in-app browser
		strings.Contains(l, " qq/") // QQ chat WebView token
}

// guardInAppWebView serves the "open in your system browser" interstitial when a WeChat
// or QQ WebView makes a top-level HTML navigation to the console, returning true when it
// handled the request. It returns false — letting the normal SPA/static path run — for
// non-navigations, non-WebView agents, the API/config/health paths, and once the visitor
// has acknowledged (via the ack cookie or the ua_ack escape hatch, which also plants the
// cookie so the acknowledgement sticks across reloads).
func guardInAppWebView(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	p := r.URL.Path
	if p == "/healthz" || p == "/readyz" || p == "/config.json" || strings.HasPrefix(p, "/api/") {
		return false
	}
	// Only intercept an actual HTML document navigation, never asset/XHR requests
	// (those do not send Accept: text/html), so an acknowledged SPA loads normally.
	if !strings.Contains(r.Header.Get("Accept"), "text/html") {
		return false
	}
	if !isInAppWebView(r.Header.Get("User-Agent")) {
		return false
	}
	if _, err := r.Cookie(webViewAckCookie); err == nil {
		return false // already acknowledged
	}
	if r.URL.Query().Get("ua_ack") == "1" {
		// The "continue anyway" path: remember the choice and let the SPA load.
		http.SetCookie(w, &http.Cookie{
			Name:     webViewAckCookie,
			Value:    "1",
			Path:     "/",
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   3600,
		})
		return false
	}
	serveWebViewInterstitial(w, r)
	return true
}

// serveWebViewInterstitial writes the guidance page (HTTP 200, self-contained, no external
// assets or JS so it renders inside a restricted WebView and triggers no dialogs).
func serveWebViewInterstitial(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_ = webViewInterstitialTmpl.Execute(w, struct {
		URL    string
		AckURL string
	}{
		URL:    externalURL(r),
		AckURL: ackURL(r),
	})
}

// externalURL rebuilds the absolute URL the visitor should paste into a system browser,
// with the internal ua_ack hint stripped. The scheme prefers X-Forwarded-Proto (the edge
// terminates TLS upstream), then the request's own TLS, defaulting to https — the console
// is an https origin.
func externalURL(r *http.Request) string {
	scheme := "https"
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	} else if r.TLS == nil {
		scheme = "https"
	}
	u := *r.URL
	q := u.Query()
	q.Del("ua_ack")
	u.RawQuery = q.Encode()
	return scheme + "://" + r.Host + u.RequestURI()
}

// ackURL is the same location with the ua_ack escape hatch set — a relative link (same
// origin), so it works regardless of the external host.
func ackURL(r *http.Request) string {
	u := *r.URL
	q := u.Query()
	q.Set("ua_ack", "1")
	u.RawQuery = q.Encode()
	return u.RequestURI()
}

// webViewInterstitialTmpl is the guidance page. html/template escapes the reflected URL
// values (defusing a reflected-XSS via a crafted path/query) in both text and URL/href
// contexts.
var webViewInterstitialTmpl = template.Must(template.New("webview").Parse(`<!DOCTYPE html>
<html lang="zh">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>请用系统浏览器打开 / Open in your browser</title>
<style>
  body { margin:0; padding:1.5rem; font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;
         background:#0f1115; color:#e6e6e6; line-height:1.6; }
  .card { max-width:32rem; margin:0 auto; background:#1a1d24; border:1px solid #2a2f3a;
          border-radius:12px; padding:1.5rem; }
  h1 { font-size:1.2rem; margin:0 0 .75rem; }
  p { margin:.5rem 0; }
  .muted { color:#9aa2b1; font-size:.9rem; }
  .url { display:block; word-break:break-all; background:#0f1115; border:1px solid #2a2f3a;
         border-radius:8px; padding:.6rem .75rem; margin:.75rem 0; font-family:ui-monospace,monospace;
         color:#8ec5ff; }
  ol { padding-left:1.2rem; }
  .ack { display:inline-block; margin-top:1rem; color:#6b7280; font-size:.85rem; text-decoration:underline; }
  hr { border:0; border-top:1px solid #2a2f3a; margin:1.25rem 0; }
</style>
</head>
<body>
  <div class="card">
    <h1>请在系统浏览器中打开</h1>
    <p>你正在微信 / QQ 的内置浏览器中打开本页面。<strong>通行密钥（Passkey）在内置浏览器中无法使用</strong>，请改用系统浏览器完成登录。</p>
    <p class="muted">复制下面的网址，粘贴到 Safari / Chrome 等系统浏览器：</p>
    <span class="url">{{.URL}}</span>
    <ol class="muted">
      <li>微信：点右上角「⋯」→「在浏览器打开」</li>
      <li>QQ：点右上角「⋯」→「用浏览器打开」</li>
    </ol>
    <hr>
    <h1>Open in your system browser</h1>
    <p>You opened this page inside the WeChat / QQ in-app browser, where <strong>passkeys (WebAuthn) do not work</strong>. Copy the address above into Safari, Chrome, or another system browser to finish signing in.</p>
    <a class="ack" href="{{.AckURL}}">仍要在此继续 / Continue here anyway</a>
  </div>
</body>
</html>`))
