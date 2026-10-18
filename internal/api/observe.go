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

// Metric labels use matched route patterns and bounded methods; SSE requests are excluded from duration histograms.

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

var defaultAccessLog = slog.New(slog.NewTextHandler(os.Stderr, nil))

const routeUnmatched = "unmatched"

type reqInfo struct {
	route     string
	principal string
}

func reqInfoFrom(ctx context.Context) *reqInfo {
	info, _ := ctx.Value(ctxKeyReqInfo).(*reqInfo)
	return info
}

func noteRoute(r *http.Request, pattern string) {
	if info := reqInfoFrom(r.Context()); info != nil {
		if _, path, ok := strings.Cut(pattern, " "); ok {
			pattern = path
		}
		info.route = pattern
	}
}

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

func methodLabel(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return m
	}
	return "OTHER"
}

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

// statusRecorder preserves flush errors and ResponseController unwrapping so SSE deadlines still reach the socket.
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
