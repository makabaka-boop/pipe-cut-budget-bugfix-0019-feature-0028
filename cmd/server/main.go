// Command server runs the raincut HTTP API.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"raincut/internal/api"
)

const defaultShutdownTimeout = 15 * time.Second

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	shutdownTimeout := defaultShutdownTimeout
	if raw := os.Getenv("SHUTDOWN_TIMEOUT"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed <= 0 {
			log.Fatalf("raincut: SHUTDOWN_TIMEOUT must be a positive duration, got %q", raw)
		}
		shutdownTimeout = parsed
	}

	listener, err := net.Listen("tcp", ":"+port)
	if err != nil {
		log.Fatalf("raincut: listen on :%s: %v", port, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, listener, api.New(), shutdownTimeout); err != nil {
		log.Fatalf("raincut: %v", err)
	}
}

// run starts the HTTP server and blocks until ctx is canceled. It then stops
// accepting new connections and waits for in-flight requests to finish. If the
// shutdown timeout expires, connections are forcibly closed and the timeout
// error is returned so the process can report failure.
func run(ctx context.Context, listener net.Listener, handler http.Handler, shutdownTimeout time.Duration) error {
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(listener)
	}()

	log.Printf("raincut: listening on %s", listener.Addr())
	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	log.Printf("raincut: termination signal received; draining in-flight requests for %s", shutdownTimeout)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		// Shutdown does not interrupt a handler that is still running. Close
		// its connections immediately and return; the caller exits non-zero
		// instead of reporting a successful, but truncated, shutdown.
		_ = srv.Close()
		return fmt.Errorf("graceful shutdown timed out after %s: %w", shutdownTimeout, err)
	}

	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Printf("raincut: in-flight requests completed; shutdown successful")
	return nil
}
