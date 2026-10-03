// remote-run submits one executable and its arguments from published Git source.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gjtiquia/remote-runners/internal/client"
	"github.com/gjtiquia/remote-runners/internal/protocol"
	"github.com/gjtiquia/remote-runners/internal/workspace"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx)
	stop()
	os.Exit(code)
}

func run(ctx context.Context) int {
	flags := flag.NewFlagSet("remote-run", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	port := flags.Int("p", 2461, "local coordinator port")
	repository := flags.String("repository", "", "checkout directory (root inferred from subdirectories)")
	remote := flags.String("remote", "", "remote name or URL (default origin)")
	branch := flags.String("branch", "", "existing local branch (default current branch)")
	timeout := flags.Duration("timeout", 30*time.Minute, "execution timeout, excludes queue wait")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "usage: remote-run [options] executable [args...]")
		fmt.Fprintln(flags.Output(), "Options precede the executable; later flags belong to the command.")
		flags.PrintDefaults()
		fmt.Fprintln(flags.Output(), "Run one executable directly; put shell composition in a project script.")
		fmt.Fprintln(flags.Output(), "Waits for completion, then copies full text output to stdout; ID/status go to stderr.")
		fmt.Fprintln(flags.Output(), "Ctrl-C/SIGTERM requests cancellation, then briefly waits for available output.")
		fmt.Fprintln(flags.Output(), "Exit codes: remote command code; 125 infrastructure; 130 cancellation; 124 timeout; 2 usage.")
	}
	if err := flags.Parse(os.Args[1:]); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if *port < 1 || *port > 65535 {
		fmt.Fprintln(os.Stderr, "remote-run: port must be 1–65535")
		return 2
	}
	if *timeout <= 0 {
		fmt.Fprintln(os.Stderr, "remote-run: timeout must be positive")
		return 2
	}
	if flags.NArg() == 0 || flags.Arg(0) == "" {
		fmt.Fprintln(os.Stderr, "usage: remote-run [options] executable [args...]")
		return 2
	}
	// Inspect only exact operator tokens. Embedded punctuation is a legitimate
	// argument; composition already consumed by the caller's shell is invisible.
	for _, arg := range flags.Args() {
		switch arg {
		case "|", "||", "|&", "&&", "&", ";", ";;", ">", ">>", "<", "<<", "<<<", "<>", "&>", "&>>", "1>", "1>>", "2>", "2>>", "2>&1", "1>&2":
			fmt.Fprintf(os.Stderr, "remote-run: literal shell operator %q is unsupported; put compound logic in a project script\n", arg)
			return 2
		}
	}
	dir, err := os.Getwd()
	if err != nil {
		return failure(err)
	}
	progress("checking Git source (clean checkout and pushed branch)")
	source, err := workspace.Preflight(ctx, dir, workspace.Overrides{Repository: *repository, Remote: *remote, Branch: *branch})
	if err != nil {
		if ctx.Err() != nil {
			return interrupted(err)
		}
		return failure(err)
	}
	progress("Git source ready: branch=%q", source.Branch)
	progress("submitting job to http://127.0.0.1:%d", *port)
	api := client.New(fmt.Sprintf("http://127.0.0.1:%d", *port))
	submitCtx, submitCancel := context.WithTimeout(ctx, 10*time.Second)
	job, err := api.Submit(submitCtx, protocol.Submission{Source: source, Args: flags.Args(), Timeout: *timeout})
	submitCancel()
	if err != nil {
		if ctx.Err() != nil {
			return interrupted(fmt.Errorf("submission interrupted; acceptance may have occurred: inspect remote-run-utils -p %d jobs before resubmitting: %w", *port, err))
		}
		return failure(fmt.Errorf("submission failed; acceptance is unknown: inspect remote-run-utils -p %d jobs before resubmitting: %w", *port, err))
	}
	progress("job ID: %s", job.ID)
	job, err = waitJob(ctx, api, job)
	cancelled := ctx.Err() != nil
	if cancelled {
		// The observation context is cancelled. Cancellation must use a fresh,
		// short context, otherwise no HTTP request can reach the coordinator.
		progress("requesting cancellation for job %s", job.ID)
		cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cancelErr := api.Cancel(cancelCtx, job.ID)
		cancel()
		if cancelErr != nil {
			progress("cancellation request failed: %v", cancelErr)
		}
		settleCtx, settleCancel := context.WithTimeout(context.Background(), 10*time.Second)
		job, err = waitJob(settleCtx, api, job)
		settleCancel()
	}
	if err != nil {
		if cancelled {
			return interrupted(fmt.Errorf("job %s: cannot collect final output; inspect remote-run-utils job/output later: %w", job.ID, err))
		}
		return failure(fmt.Errorf("job %s: %w (inspect remote-run-utils job/output)", job.ID, err))
	}
	outputCtx, outputCancel := context.WithCancel(ctx)
	if cancelled {
		outputCancel()
		outputCtx, outputCancel = context.WithTimeout(context.Background(), 10*time.Second)
	}
	defer outputCancel()
	progress("fetching completed output for job %s", job.ID)
	if err = api.Output(outputCtx, job.ID, os.Stdout); err != nil {
		if cancelled || ctx.Err() != nil {
			return interrupted(fmt.Errorf("job %s: output transfer interrupted: %w", job.ID, err))
		}
		return failure(err)
	}
	if job.Error != "" {
		progress("job %s: %s", job.ID, job.Error)
	}
	code := 125
	switch {
	case cancelled || job.State == "cancelled":
		code = 130
	case job.State == "timed_out":
		code = 124
	case job.ExitCode != nil:
		code = *job.ExitCode
	default:
		progress("job %s ended %s without an exit code", job.ID, job.State)
	}
	progress("exit code: %d", code)
	return code
}

func waitJob(ctx context.Context, api *client.Client, job protocol.Job) (protocol.Job, error) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	started := time.Now()
	nextProgress := started.Add(10 * time.Second)
	logJobState(job)
	for !job.Terminal() {
		select {
		case <-ctx.Done():
			return job, ctx.Err()
		case <-ticker.C:
		}
		requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		next, err := api.Job(requestCtx, job.ID)
		cancel()
		if err != nil {
			return job, err
		}
		if next.State != job.State || next.RunnerID != job.RunnerID {
			logJobState(next)
			nextProgress = time.Now().Add(10 * time.Second)
		} else if !next.Terminal() && !time.Now().Before(nextProgress) {
			progress("job %s still %s on runner=%q (waiting %s; output appears on completion)", next.ID, next.State, next.RunnerID, time.Since(started).Round(time.Second))
			nextProgress = time.Now().Add(10 * time.Second)
		}
		job = next
	}
	return job, nil
}
func interrupted(err error) int {
	progress("%v", err)
	progress("exit code: 130")
	return 130
}
func failure(err error) int { progress("%v", err); return 125 }

func logJobState(job protocol.Job) {
	switch job.State {
	case "queued":
		progress("job %s queued; waiting for an available runner", job.ID)
	case "running":
		progress("job %s running on runner=%q; waiting for completed output", job.ID, job.RunnerID)
	default:
		progress("job %s finished: state=%s runner=%q", job.ID, job.State, job.RunnerID)
	}
}

// Progress is stderr-only; stdout remains the completed command payload.
func progress(format string, args ...any) {
	log.New(os.Stderr, "remote-run: ", log.LstdFlags).Printf(format, args...)
}
