package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

type terminal struct{ binary, socket, session, window string }

func validateTerminal(ctx context.Context) (terminal, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	value := os.Getenv("TMUX")
	pane := os.Getenv("TMUX_PANE")
	// Parse from the right: socket paths can contain commas.
	i := strings.LastIndex(value, ",")
	if i >= 0 {
		value = value[:i]
		i = strings.LastIndex(value, ",")
	}
	if i < 0 || pane == "" {
		return terminal{}, fmt.Errorf("run remote-runner inside a dedicated tmux session")
	}
	binary, err := exec.LookPath("tmux")
	if err != nil {
		return terminal{}, fmt.Errorf("validate tmux: %w", err)
	}
	term := terminal{binary: binary, socket: value[:i]}
	out, err := term.command(ctx, "display-message", "-p", "-t", pane, "#{session_id}\t#{window_id}").Output()
	if err != nil {
		return terminal{}, fmt.Errorf("validate tmux pane: %w", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 || !strings.HasPrefix(fields[0], "$") || !strings.HasPrefix(fields[1], "@") {
		return terminal{}, fmt.Errorf("validate tmux: invalid session/window IDs")
	}
	term.session, term.window = fields[0], fields[1]
	return term, nil
}
func (t terminal) command(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, t.binary, append([]string{"-S", t.socket}, args...)...)
}
