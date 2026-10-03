// remote-runner-server hosts the network-facing runner transport and the
// loopback-only management API. All coordinator state is ephemeral.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gjtiquia/remote-runners/internal/coordinator"
)

var errUsage = errors.New("invalid command usage")

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "remote-runner-server:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

func run() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("locate home directory: %w", err)
	}
	flags := flag.NewFlagSet("remote-runner-server", flag.ContinueOnError)
	port := flags.Int("p", 2461, "TCP port (runner connections network-wide; management loopback-only)")
	outputDir := flags.String("output-dir", filepath.Join(home, ".remote-runners", "output"), "directory for temporary disk-backed job output")
	heartbeatInterval := flags.Duration("heartbeat-interval", 10*time.Second, "interval between coordinator heartbeat requests")
	heartbeatMisses := flags.Int("heartbeat-misses", 3, "consecutive missed heartbeat responses before deregistration")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "usage: remote-runner-server [options]")
		flags.PrintDefaults()
		fmt.Fprintln(flags.Output(), "Port 0 selects an ephemeral port; the bound address is logged to stderr.")
		fmt.Fprintln(flags.Output(), "SIGINT/SIGTERM gracefully close HTTP, runner sockets and owned output spools.")
		fmt.Fprintln(flags.Output(), "Coordinator restart forgets registry, queue and history; remote jobs may continue.")
		fmt.Fprintln(flags.Output(), "Exit codes: 0 help/graceful shutdown; 1 infrastructure failure; 2 invalid usage.")
	}
	if err := flags.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("%w: %s", errUsage, err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%w: server takes options only, not positional arguments", errUsage)
	}
	if *port < 0 || *port > 65535 {
		return fmt.Errorf("%w: port must be between 0 and 65535", errUsage)
	}
	if *heartbeatInterval <= 0 {
		return fmt.Errorf("%w: heartbeat interval must be positive", errUsage)
	}
	if *heartbeatMisses < 1 {
		return fmt.Errorf("%w: heartbeat misses must be at least 1", errUsage)
	}
	if strings.TrimSpace(*outputDir) == "" {
		return fmt.Errorf("%w: output directory must not be empty", errUsage)
	}
	s, err := coordinator.New(coordinator.Options{OutputDir: *outputDir, HeartbeatInterval: *heartbeatInterval, MissLimit: *heartbeatMisses})
	if err != nil {
		return fmt.Errorf("create coordinator: %w", err)
	}
	defer s.Close()
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	httpServer := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	served := make(chan error, 1)
	go func() { served <- httpServer.Serve(listener) }()
	logger := log.New(os.Stderr, "remote-runner-server: ", log.LstdFlags)
	logger.Println("remote-runner-server listening on", listener.Addr())
	select {
	case err := <-served:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve: %w", err)
		}
	case <-ctx.Done():
		logger.Print("shutdown requested; draining HTTP")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownErr := httpServer.Shutdown(shutdownCtx)
		// Shutdown does not close hijacked WebSockets; the coordinator owns them.
		closeErr := s.Close()
		if shutdownErr != nil {
			_ = httpServer.Close()
		}
		logger.Printf("shutdown complete cleanup_ok=%t", shutdownErr == nil && closeErr == nil)
		return errors.Join(shutdownErr, closeErr)
	}
	return s.Close()
}
