package panel

import (
	"html/template"
	"net/http"
	"strings"
)

// Guide WeChat/QQ WebViews to a system browser because they cannot complete passkey enrollment.

const webViewAckCookie = "felis_ua_ack"

func isInAppWebView(ua string) bool {
	if ua == "" {
		return false
	}
	l := strings.ToLower(ua)
	return strings.Contains(l, "micromessenger") ||
		strings.Contains(l, "mqqbrowser") ||
		strings.Contains(l, " qq/")
}

// guardInAppWebView intercepts HTML navigation until the visitor acknowledges the warning.
func guardInAppWebView(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	p := r.URL.Path
	if p == "/healthz" || p == "/readyz" || p == "/config.json" || strings.HasPrefix(p, "/api/") {
		return false
	}
	// Only intercept HTML documents so assets and API requests remain available.
	if !strings.Contains(r.Header.Get("Accept"), "text/html") {
		return false
	}
	if !isInAppWebView(r.Header.Get("User-Agent")) {
		return false
	}
	if _, err := r.Cookie(webViewAckCookie); err == nil {
		return false
	}
	if r.URL.Query().Get("ua_ack") == "1" {
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

// externalURL strips ua_ack and prefers the edge scheme from X-Forwarded-Proto.
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

func ackURL(r *http.Request) string {
	u := *r.URL
	q := u.Query()
	q.Set("ua_ack", "1")
	u.RawQuery = q.Encode()
	return u.RequestURI()
}

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
