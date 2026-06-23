package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"strconv"
	"time"

	"boot.dev/linko/internal/store"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	pkgerr "github.com/pkg/errors"
)

// httpRequestsTotal counts requests by method, path and status.
var httpRequestsTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Total number of HTTP requests.",
	},
	[]string{"method", "path", "status"},
)

type server struct {
	httpServer *http.Server
	store      store.Store
	cancel     context.CancelFunc
	logger     *slog.Logger
}

// LogContext holds contextual information for request logging
type LogContext struct {
	Username string
	Error    error
}

const logContextKey = "log_context"

func newServer(store store.Store, port int, cancel context.CancelFunc, logger *slog.Logger) *server {
	mux := http.NewServeMux()

	s := &server{
		store:  store,
		cancel: cancel,
		logger: logger,
	}

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: requestLogger(logger)(requestIDMiddleware(metricsMiddleware(otelhttp.NewHandler(mux, "http.server")))),
	}
	s.httpServer = srv

	mux.Handle("GET /metrics", promhttp.Handler())
	mux.Handle("GET /debug/pprof/", s.authMiddleware(http.HandlerFunc(pprof.Index)))
	mux.Handle("GET /debug/pprof/profile", s.authMiddleware(http.HandlerFunc(pprof.Profile)))
	mux.HandleFunc("GET /", s.handlerIndex)
	mux.Handle("POST /api/login", s.authMiddleware(http.HandlerFunc(s.handlerLogin)))
	mux.Handle("POST /api/shorten", s.authMiddleware(http.HandlerFunc(s.handlerShortenLink)))
	mux.Handle("GET /api/stats", s.authMiddleware(http.HandlerFunc(s.handlerStats)))
	mux.Handle("GET /api/urls", s.authMiddleware(http.HandlerFunc(s.handlerListURLs)))
	mux.HandleFunc("GET /{shortCode}", s.handlerRedirect)
	mux.HandleFunc("POST /admin/shutdown", s.handlerShutdown)

	return s
}

func redactIP(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}

	ip := net.ParseIP(host)
	if ip == nil {
		return addr
	}

	ipv4 := ip.To4()
	if ipv4 == nil {
		return addr
	}

	redactedHost := fmt.Sprintf("%d.%d.%d.x", ipv4[0], ipv4[1], ipv4[2])
	if port != "" {
		return net.JoinHostPort(redactedHost, port)
	}
	return redactedHost
}

func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			// Wrap request body to count bytes read
			var spyBody *spyReadCloser
			if r.Body != nil {
				spyBody = newSpyReadCloser(r.Body)
				r.Body = spyBody
			}

			// Wrap response writer to capture status and bytes written
			spyW := newSpyResponseWriter(w)

			// Create LogContext and store it on the request context
			logCtx := &LogContext{}
			r = r.WithContext(context.WithValue(r.Context(), logContextKey, logCtx))

			next.ServeHTTP(spyW, r)

			// Get request body bytes read
			var requestBodyBytes int64
			if spyBody != nil {
				requestBodyBytes = spyBody.read
			}

			// Build log attributes
			logAttrs := []any{
				"method", r.Method,
				"path", r.URL.Path,
				"client_ip", redactIP(r.RemoteAddr),
				"duration", time.Since(start).String(),
				"request_body_bytes", requestBodyBytes,
				"response_status", spyW.statusCode,
				"response_body_bytes", spyW.written,
				"request_id", spyW.Header().Get("X-Request-ID"),
			}

			// Add user attribute if username is set
			if logCtx.Username != "" {
				logAttrs = append(logAttrs, "user", logCtx.Username)
			}

			// Add error attribute if present
			if logCtx.Error != nil {
				logAttrs = append(logAttrs, "error", logCtx.Error)
			}

			logger.Info("Served request", logAttrs...)
		})
	}
}

// spyReadCloser wraps an io.ReadCloser and counts bytes read
type spyReadCloser struct {
	rc   io.ReadCloser
	read int64
}

func newSpyReadCloser(rc io.ReadCloser) *spyReadCloser {
	return &spyReadCloser{rc: rc}
}

func (s *spyReadCloser) Read(p []byte) (n int, err error) {
	n, err = s.rc.Read(p)
	s.read += int64(n)
	return n, err
}

func (s *spyReadCloser) Close() error {
	return s.rc.Close()
}

// spyResponseWriter wraps http.ResponseWriter to capture status code and bytes written
type spyResponseWriter struct {
	http.ResponseWriter
	statusCode int
	written    int64
}

func newSpyResponseWriter(w http.ResponseWriter) *spyResponseWriter {
	return &spyResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
}

func (s *spyResponseWriter) WriteHeader(statusCode int) {
	s.statusCode = statusCode
	s.ResponseWriter.WriteHeader(statusCode)
}

func (s *spyResponseWriter) Write(p []byte) (n int, err error) {
	n, err = s.ResponseWriter.Write(p)
	s.written += int64(n)
	return n, err
}

func (s *server) start() error {
	s.logger.Debug(fmt.Sprintf("Linko is running on http://localhost%s", s.httpServer.Addr))
	ln, err := net.Listen("tcp", s.httpServer.Addr)
	if err != nil {
		return err
	}
	if err := s.httpServer.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *server) shutdown(ctx context.Context) error {
	s.logger.Debug("Linko is shutting down")
	return s.httpServer.Shutdown(ctx)
}

// httpError stashes the error (wrapped with stack trace) in LogContext and sends an HTTP error response
func httpError(ctx context.Context, w http.ResponseWriter, statusCode int, err error) {
	wrappedErr := pkgerr.WithStack(err)
	if logCtx, ok := ctx.Value(logContextKey).(*LogContext); ok {
		logCtx.Error = wrappedErr
	}

	responseBody := err.Error()
	if statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden || statusCode == http.StatusInternalServerError {
		responseBody = http.StatusText(statusCode)
	}
	http.Error(w, responseBody, statusCode)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{
			ResponseWriter: w,
			status:         http.StatusOK,
		}

		next.ServeHTTP(rec, r)

		path := r.URL.Path
		method := r.Method
		status := strconv.Itoa(rec.status)

		httpRequestsTotal.
			WithLabelValues(method, path, status).
			Inc()
	})
}

// requestIDMiddleware reads X-Request-ID from the request or generates one,
// and sets it on the response header before calling the next handler.
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-ID")
		if requestID == "" {
			requestID = rand.Text()
		}
		w.Header().Set("X-Request-ID", requestID)
		next.ServeHTTP(w, r)
	})
}

func (s *server) handlerShutdown(w http.ResponseWriter, r *http.Request) {
	if os.Getenv("ENV") == "production" {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusOK)
	go s.cancel()
}
