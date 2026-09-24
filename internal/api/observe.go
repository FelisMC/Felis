package api

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Request observability: one access-log line and two series per API request.
//
// The route label is the matched ServeMux pattern ("/api/v1/servers/{name}"),
// never the raw path, so a scan of random URLs cannot grow the series set; a
// request no route matched is "unmatched". The method label is folded the same
// way (methodLabel). Stream routes (text/event-stream) count toward the request
// total but stay out of the duration histogram, where an attachment that lasts
// half an hour would only bury the latency of everything else.

var (
	httpRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "felis",
		Name:      "http_requests_total",
		Help:      "API requests served, by face, method, route pattern and status code.",
	}, []string{"face", "method", "route", "code"})

	httpRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "felis",
		Name:      "http_request_duration_seconds",
		Help:      "API request latency, by face and route pattern (streams excluded).",
		Buckets:   []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
	}, []string{"face", "route"})
)

// defaultAccessLog writes logfmt lines to stderr, next to the API's other logs.
var defaultAccessLog = slog.New(slog.NewTextHandler(os.Stderr, nil))

// routeUnmatched labels a request no route pattern matched.
const routeUnmatched = "unmatched"

// reqInfo is filled in by the handlers a request passes through, for observe to
// read once the response is done: the matched route, and the principal once the
// face's guard has resolved one.
type reqInfo struct {
	route     string
	principal string
}

func reqInfoFrom(ctx context.Context) *reqInfo {
	info, _ := ctx.Value(ctxKeyReqInfo).(*reqInfo)
	return info
}

// noteRoute records the matched route pattern (without its method).
func noteRoute(r *http.Request, pattern string) {
	if info := reqInfoFrom(r.Context()); info != nil {
		if _, path, ok := strings.Cut(pattern, " "); ok {
			pattern = path
		}
		info.route = pattern
	}
}

// tagRoute wraps a route's handler so the request records its pattern and, on
// the authenticated faces, who made it.
func tagRoute(pattern string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noteRoute(r, pattern)
		if info := reqInfoFrom(r.Context()); info != nil {
			if p := principalFromContext(r.Context()); p != nil {
				info.principal = p.UserID
			}
		}
		h(w, r)
	}
}

// methodLabel folds anything but the methods the API serves into one value.
func methodLabel(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return m
	}
	return "OTHER"
}

// quietRoute reports whether a successful request is left out of the access log:
// the probes and the scrape, which arrive every few seconds and say nothing.
func quietRoute(route string, status int) bool {
	if status >= 400 {
		return false
	}
	switch route {
	case "/healthz", "/readyz", "/metrics":
		return true
	}
	return false
}

// observe records every request that reaches face: the access-log line and the
// felis_http_* series.
func (a *API) observe(face string, next http.Handler) http.Handler {
	logger := a.AccessLog
	if logger == nil {
		logger = defaultAccessLog
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		info := &reqInfo{}
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), ctxKeyReqInfo, info)))

		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		route := info.route
		if route == "" {
			route = routeUnmatched
		}
		elapsed := time.Since(start)
		httpRequestsTotal.WithLabelValues(face, methodLabel(r.Method), route, strconv.Itoa(status)).Inc()
		if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/event-stream") {
			httpRequestDuration.WithLabelValues(face, route).Observe(elapsed.Seconds())
		}
		if quietRoute(route, status) {
			return
		}
		logger.LogAttrs(r.Context(), slog.LevelInfo, "request",
			slog.String("face", face),
			slog.String("method", r.Method),
			slog.String("route", route),
			slog.String("path", r.URL.Path),
			slog.Int("status", status),
			slog.Int64("duration_ms", elapsed.Milliseconds()),
			slog.Int64("bytes", rec.bytes),
			slog.String("request_id", requestIDFromContext(r.Context())),
			slog.String("principal", info.principal),
		)
	})
}

// statusRecorder notes the status and size of a response. It passes flushes
// through with their error (the SSE relay's write-deadline guard depends on
// seeing a failed flush) and unwraps for http.ResponseController, so deadlines
// still reach the connection.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(p []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(p)
	s.bytes += int64(n)
	return n, err
}

func (s *statusRecorder) FlushError() error {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return http.NewResponseController(s.ResponseWriter).Flush()
}

func (s *statusRecorder) Flush() { _ = s.FlushError() }

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
