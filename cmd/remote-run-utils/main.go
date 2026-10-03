// remote-run-utils inspects and manages the coordinator over IPv4 loopback.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/gjtiquia/remote-runners/internal/client"
)

var errUsage = errors.New("invalid command usage")

const usage = "usage: remote-run-utils [-p PORT] {runners|jobs|job ID|output ID|cancel ID}"

func usageError(message string) error {
	return fmt.Errorf("%w: %s\n%s\nOptions must precede the subcommand.", errUsage, message, usage)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "remote-run-utils:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

func run() error {
	flags := flag.NewFlagSet("remote-run-utils", flag.ContinueOnError)
	port := flags.Int("p", 2461, "coordinator TCP port on 127.0.0.1 (options precede subcommand)")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), usage)
		fmt.Fprintln(flags.Output(), "Options must precede the subcommand.")
		fmt.Fprintln(flags.Output(), "  runners     JSON array of runner identity, priority, capacity and availability")
		fmt.Fprintln(flags.Output(), "  jobs        JSON array of queued/running/completed jobs")
		fmt.Fprintln(flags.Output(), "  job ID      JSON job details")
		fmt.Fprintln(flags.Output(), "  output ID   raw completed text output to stdout")
		fmt.Fprintln(flags.Output(), "  cancel ID   request cancellation; no stdout on success")
		flags.PrintDefaults()
		fmt.Fprintln(flags.Output(), "Exit codes: 0 success/help; 1 coordinator/network/output failure; 2 invalid usage.")
	}
	if err := flags.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return usageError(err.Error())
	}
	if *port < 1 || *port > 65535 {
		return usageError("port must be between 1 and 65535")
	}
	args := flags.Args()
	if len(args) == 0 {
		return usageError("a subcommand is required")
	}
	c := client.New(fmt.Sprintf("http://127.0.0.1:%d", *port))
	ctx := context.Background()
	switch args[0] {
	case "runners":
		if len(args) != 1 {
			return usageError("runners takes no arguments")
		}
		runners, err := c.Runners(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(runners)
	case "output":
		if len(args) != 2 || strings.TrimSpace(args[1]) == "" || strings.HasPrefix(args[1], "-") {
			return usageError("output requires one job ID")
		}
		if err := c.Output(ctx, args[1], os.Stdout); err != nil {
			return fmt.Errorf("output %s: %w", args[1], err)
		}
		return nil
	case "cancel":
		if len(args) != 2 || strings.TrimSpace(args[1]) == "" || strings.HasPrefix(args[1], "-") {
			return usageError("cancel requires one job ID")
		}
		if err := c.Cancel(ctx, args[1]); err != nil {
			return fmt.Errorf("cancel %s: %w", args[1], err)
		}
		return nil
	case "job":
		if len(args) != 2 || strings.TrimSpace(args[1]) == "" || strings.HasPrefix(args[1], "-") {
			return usageError("job requires one job ID")
		}
		job, err := c.Job(ctx, args[1])
		if err != nil {
			return fmt.Errorf("job %s: %w", args[1], err)
		}
		return json.NewEncoder(os.Stdout).Encode(job)
	case "jobs":
		if len(args) != 1 {
			return usageError("jobs takes no arguments")
		}
		jobs, err := c.Jobs(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(jobs)
	default:
		return usageError(fmt.Sprintf("unknown subcommand %q", args[0]))
	}
}
