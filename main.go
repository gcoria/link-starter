package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"boot.dev/linko/internal/build"
	"boot.dev/linko/internal/linkoerr"
	"boot.dev/linko/internal/store"
	"github.com/lmittmann/tint"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"github.com/mattn/go-isatty"
	pkgerr "github.com/pkg/errors"
	"gopkg.in/natefinch/lumberjack.v2"
)

type closeFunc func() error

type stackTracer interface {
	error
	StackTrace() pkgerr.StackTrace
}

type multiError interface {
	error
	Unwrap() []error
}

var (
	sensitiveKeys = []string{"password", "key", "apikey", "secret", "pin", "creditcardno", "user"}
	tracer        trace.Tracer
)

// errorAttrs builds a slice of slog.Attr for a single error containing:
// - message attribute with the error's message
// - any linkoerr attributes extracted from the error
// - stack_trace attribute (only if the error is a stackTracer)
func errorAttrs(err error) []slog.Attr {
	attrs := []slog.Attr{slog.String("message", err.Error())}

	// Extract and add structured attributes from error chain
	errAttrs := linkoerr.Attrs(err)
	if len(errAttrs) > 0 {
		attrs = append(attrs, errAttrs...)
	}

	// Add stack trace if present
	var stackErr stackTracer
	if errors.As(err, &stackErr) {
		attrs = append(attrs, slog.String("stack_trace", fmt.Sprintf("%+v", stackErr.StackTrace())))
	}

	return attrs
}

func redactURLPassword(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return s
	}
	if _, ok := u.User.Password(); !ok {
		return s
	}
	u.User = url.UserPassword(u.User.Username(), "[REDACTED]")
	return u.String()
}

func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	if slices.Contains(sensitiveKeys, a.Key) {
		return slog.String(a.Key, "[REDACTED]")
	}
	if a.Value.Kind() == slog.KindString {
		if redacted := redactURLPassword(a.Value.String()); redacted != a.Value.String() {
			return slog.String(a.Key, redacted)
		}
	}
	if a.Key == "error" {
		err, ok := a.Value.Any().(error)
		if !ok {
			return a
		}

		// Check for multi-error and handle it specially
		var multiErr multiError
		if errors.As(err, &multiErr) {
			// Build grouped errors for multi-error
			wrappedErrs := multiErr.Unwrap()
			errorGroups := make([]slog.Attr, 0, len(wrappedErrs))
			for i, wrappedErr := range wrappedErrs {
				key := fmt.Sprintf("error_%d", i+1)
				errorGroups = append(errorGroups, slog.GroupAttrs(key, errorAttrs(wrappedErr)...))
			}
			return slog.GroupAttrs("errors", errorGroups...)
		}

		// Single error case - use existing "error" key
		return slog.GroupAttrs(a.Key, errorAttrs(err)...)
	}
	return a
}

func initTracing(ctx context.Context) (func(context.Context) error, error) {
	exp, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp,
			sdktrace.WithBatchTimeout(2*time.Second),
		),
		sdktrace.WithResource(resource.Default()),
	)

	otel.SetTracerProvider(tp)
	tracer = tp.Tracer("boot.dev/linko")
	return tp.Shutdown, nil
}

func initializeLogger(logFile string) (*slog.Logger, closeFunc, error) {
	// STDERR handler - DEBUG and above with color support
	noColor := !isatty.IsTerminal(os.Stderr.Fd()) && !isatty.IsCygwinTerminal(os.Stderr.Fd())
	stderrHandler := tint.NewHandler(os.Stderr, &tint.Options{
		Level:       slog.LevelDebug,
		ReplaceAttr: replaceAttr,
		NoColor:     noColor,
	})

	if logFile != "" {
		logger := &lumberjack.Logger{
			Filename:   logFile,
			MaxSize:    1,
			MaxAge:     28,
			MaxBackups: 10,
			LocalTime:  false,
			Compress:   true,
		}

		// File handler - INFO and above (JSON format)
		fileHandler := slog.NewJSONHandler(logger, &slog.HandlerOptions{
			Level:       slog.LevelInfo,
			ReplaceAttr: replaceAttr,
		})

		// Combine both handlers
		multiHandler := slog.NewMultiHandler(stderrHandler, fileHandler)

		closeLogger := func() error {
			return logger.Close()
		}
		return slog.New(multiHandler), closeLogger, nil
	}
	return slog.New(stderrHandler), func() error { return nil }, nil
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	httpPort := flag.Int("port", 8899, "port to listen on")
	dataDir := flag.String("data", "./data", "directory to store data")
	flag.Parse()

	status := run(ctx, cancel, *httpPort, *dataDir)
	cancel()
	os.Exit(status)
}

func run(ctx context.Context, cancel context.CancelFunc, httpPort int, dataDir string) int {
	shutdownTracing, err := initTracing(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize tracing: %v\n", err)
		return 1
	}
	defer func() {
		if err := shutdownTracing(context.Background()); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to shutdown tracing: %v\n", err)
		}
	}()

	logger, closeLogger, err := initializeLogger(os.Getenv("LINKO_LOG_FILE"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize logger: %v\n", err)
		return 1
	}
	hostname, _ := os.Hostname()
	logger = logger.With(
		slog.String("git_sha", build.GitSHA),
		slog.String("build_time", build.BuildTime),
		slog.String("env", os.Getenv("ENV")),
		slog.String("hostname", hostname),
	)
	defer func() {
		if err := closeLogger(); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to close logger: %v\n", err)
		}
	}()

	st, err := store.New(dataDir, logger)
	if err != nil {
		logger.Error("failed to create store", "error", err)
		return 1
	}
	s := newServer(*st, httpPort, cancel, logger)
	var serverErr error
	go func() {
		serverErr = s.start()
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	logger.Debug("Linko is shutting down")
	if err := s.shutdown(shutdownCtx); err != nil {
		logger.Error("failed to shutdown server", "error", err)
		return 1
	}
	if serverErr != nil {
		logger.Error("server error", "error", serverErr)
		return 1
	}
	return 0
}
