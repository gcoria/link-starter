package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"boot.dev/linko/internal/build"
	"boot.dev/linko/internal/linkoerr"
	"boot.dev/linko/internal/store"
	pkgerr "github.com/pkg/errors"
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

func replaceAttr(groups []string, a slog.Attr) slog.Attr {
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

func initializeLogger(logFile string) (*slog.Logger, closeFunc, error) {
	// STDERR handler - DEBUG and above
	stderrHandler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level:       slog.LevelDebug,
		ReplaceAttr: replaceAttr,
	})

	if logFile != "" {
		file, err := os.OpenFile(logFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to open log file: %w", err)
		}
		bufferedFile := bufio.NewWriterSize(file, 8192)

		// File handler - INFO and above (JSON format)
		fileHandler := slog.NewJSONHandler(bufferedFile, &slog.HandlerOptions{
			Level:       slog.LevelInfo,
			ReplaceAttr: replaceAttr,
		})

		// Combine both handlers
		multiHandler := slog.NewMultiHandler(stderrHandler, fileHandler)

		closeLogger := func() error {
			if err := bufferedFile.Flush(); err != nil {
				return fmt.Errorf("failed to flush log file: %w", err)
			}
			if err := file.Close(); err != nil {
				return fmt.Errorf("failed to close log file: %w", err)
			}
			return nil
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
