package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"boot.dev/linko/internal/store"
)

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
	accessFile, err := os.OpenFile("linko.access.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Printf("failed to open access log: %v\n", err)
		return 1
	}
	defer accessFile.Close()

	accessLogger := log.New(accessFile, "INFO: ", log.LstdFlags)
	stdLogger := log.New(os.Stderr, "DEBUG: ", log.LstdFlags)

	st, err := store.New(dataDir, stdLogger)
	if err != nil {
		stdLogger.Printf("failed to create store: %v\n", err)
		return 1
	}
	s := newServer(*st, httpPort, cancel, accessLogger)
	var serverErr error
	go func() {
		serverErr = s.start()
	}()

	<-ctx.Done()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	if err := s.shutdown(shutdownCtx); err != nil {
		stdLogger.Printf("failed to shutdown server: %v\n", err)
		return 1
	}
	if serverErr != nil {
		stdLogger.Printf("server error: %v\n", serverErr)
		return 1
	}
	return 0
}
