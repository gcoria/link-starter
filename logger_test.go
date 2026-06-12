package main

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func Test_requestLogger(t *testing.T) {
	logBuffer := &bytes.Buffer{}

	logger := slog.New(slog.NewTextHandler(logBuffer, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Time(slog.TimeKey, time.Date(2023, 10, 1, 12, 34, 57, 0, time.UTC))
			}
			if a.Key == "duration" {
				return slog.String("duration", "0s")
			}
			return a
		},
	}))

	requestLoggerMiddleware := requestLogger(logger)
	dummyHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	// Wrap with requestIDMiddleware first, then requestLogger, matching the actual server setup
	loggedHandler := requestIDMiddleware(requestLoggerMiddleware(dummyHandler))

	req := httptest.NewRequest("GET", "http://lin.ko/api/stats", nil)
	rr := httptest.NewRecorder()
	loggedHandler.ServeHTTP(rr, req)

	expectedLogString := `time=2023-10-01T12:34:57.000Z level=INFO msg="Served request" method=GET path=/api/stats client_ip=192.0.2.x:1234 duration=0s request_body_bytes=0 response_status=200 response_body_bytes=0 request_id=` + rr.Header().Get("X-Request-ID") + "\n"
	const expectedStatusCode = http.StatusOK

	if rr.Code != expectedStatusCode {
		t.Errorf("Expected status code %d, got %d", expectedStatusCode, rr.Code)
	}
	if logBuffer.String() != expectedLogString {
		t.Errorf("Expected log output:\n%s\nGot:\n%s", expectedLogString, logBuffer.String())
	}
}
