package runner

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

// Main is the worker command entrypoint; job-exec is a private subprocess protocol.
func Main(args []string) int {
	if len(args) == 2 && args[0] == "job-exec" {
		code, err := executeJob(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return code
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "usage: remote-runner")
		return 1
	}
	cfg, err := LoadConfig(filepath.Join(home, ".remote-runner", "config.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err = Run(ctx, cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
