package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"boot.dev/linko/internal/store"
)

type server struct {
	httpServer *http.Server
	store      store.Store
	cancel     context.CancelFunc
	logger     *slog.Logger
}

func newServer(store store.Store, port int, cancel context.CancelFunc, logger *slog.Logger) *server {
	mux := http.NewServeMux()

	s := &server{
		store:  store,
		cancel: cancel,
		logger: logger,
	}

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: requestLogger(logger)(mux),
	}
	s.httpServer = srv

	mux.HandleFunc("GET /", s.handlerIndex)
	mux.Handle("POST /api/login", s.authMiddleware(http.HandlerFunc(s.handlerLogin)))
	mux.Handle("POST /api/shorten", s.authMiddleware(http.HandlerFunc(s.handlerShortenLink)))
	mux.Handle("GET /api/stats", s.authMiddleware(http.HandlerFunc(s.handlerStats)))
	mux.Handle("GET /api/urls", s.authMiddleware(http.HandlerFunc(s.handlerListURLs)))
	mux.HandleFunc("GET /{shortCode}", s.handlerRedirect)
	mux.HandleFunc("POST /admin/shutdown", s.handlerShutdown)

	return s
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

			next.ServeHTTP(spyW, r)

			// Get request body bytes read
			var requestBodyBytes int64
			if spyBody != nil {
				requestBodyBytes = spyBody.read
			}

			logger.Info("Served request",
				"method", r.Method,
				"path", r.URL.Path,
				"client_ip", r.RemoteAddr,
				"duration", time.Since(start).String(),
				"request_body_bytes", requestBodyBytes,
				"response_status", spyW.statusCode,
				"response_body_bytes", spyW.written,
			)
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

func (s *server) handlerShutdown(w http.ResponseWriter, r *http.Request) {
	if os.Getenv("ENV") == "production" {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusOK)
	go s.cancel()
}
