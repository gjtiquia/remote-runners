package runner

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const shellStateOption = "@remote-runner-shell-state"
const shellPIDOption = "@remote-runner-shell-pid"
const shellJobOption = "@remote-runner-job-active"

// ErrShellBusy proves that no command was sent; transport failures are uncertain.
var ErrShellBusy = errors.New("worktree window is busy")

var errShellLaunchUncertain = errors.New("interactive shell launch outcome is uncertain")

// newShellWindow installs hooks through private startup files, never by typing
// bootstrap commands into a pane which may already be running a startup command.
// The interactive shell's PID is tmux's pane_pid throughout
// its lifetime, and jobs return to that same shell rather than replacing it.
func (t terminal) newShellWindow(name, dir string, environment []string) (*jobWindow, error) {
	out, err := t.output("show-option", "-A", "-v", "-t", t.session, "default-shell")
	if err != nil {
		return nil, fmt.Errorf("read tmux default-shell: %w", err)
	}
	shell := strings.TrimSpace(string(out))
	kind := filepath.Base(shell)
	if !filepath.IsAbs(shell) || (kind != "bash" && kind != "zsh") {
		return nil, fmt.Errorf("unsupported tmux default-shell: set tmux default-shell to Zsh or Bash 5.1 or newer")
	}
	if kind == "bash" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		check := exec.CommandContext(ctx, shell, "--noprofile", "--norc", "-c", `(( BASH_VERSINFO[0] > 5 || (BASH_VERSINFO[0] == 5 && BASH_VERSINFO[1] >= 1) ))`)
		check.Env = []string{} // In particular, do not execute an inherited BASH_ENV.
		err = check.Run()
		cancel()
		if err != nil {
			return nil, fmt.Errorf("interactive job windows require Bash 5.1 or newer: set tmux default-shell to Zsh or a modern Bash")
		}
	}
	binary, err := filepath.Abs(t.binary)
	if err != nil {
		return nil, err
	}
	startup, err := os.MkdirTemp(dir, "shell-")
	if err != nil {
		return nil, err
	}
	startup, err = filepath.Abs(startup)
	if err != nil {
		return nil, err
	}
	token := rand.Text()
	// As with a normal tmux shell, use the pane environment. Explicit -e values
	// override stale runner-baseline values in the tmux server; startup and manual
	// exports then take precedence. TMUX, TMUX_PANE and TERM belong to the pane.
	// Launch the shell directly, without an intermediate shell filtering unusual
	// environment keys such as Bash's exported-function entries.
	args := []string{"new-window", "-d", "-t", t.session + ":", "-n", name, "-c", filepath.Dir(filepath.Dir(dir)), "-P", "-F", "#{window_id}\t#{pane_id}"}
	var zdotdir string
	var hasZdotdir bool
	for _, entry := range environment {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "TMUX" || key == "TMUX_PANE" || key == "TERM" {
			continue
		}
		args = append(args, "-e", entry)
		if key == "ZDOTDIR" {
			zdotdir, hasZdotdir = value, true
		}
	}
	// Each hook invocation targets this server and its own pane explicitly, not
	// the runner's inherited pane or whichever server happens to be the default.
	tmux := "command " + quote(binary) + " -S " + quote(t.socket)
	common := `
` + tmux + ` set-option -p -t "$TMUX_PANE" ` + shellJobOption + ` 0 >/dev/null 2>&1
__rr_busy() {
  local __rr_status=$?
  ` + tmux + ` set-option -p -t "$TMUX_PANE" ` + shellStateOption + " " + quote(token+":busy") + ` >/dev/null 2>&1
  return "$__rr_status"
}
__rr_idle() {
  ` + tmux + ` set-option -p -t "$TMUX_PANE" ` + shellPIDOption + ` "$$" \; set-option -p -t "$TMUX_PANE" ` + shellStateOption + " " + quote(token+":idle") + ` >/dev/null 2>&1
}
`
	write := func(name, text string) error {
		return os.WriteFile(filepath.Join(startup, name), []byte(text), 0600)
	}
	if kind == "bash" {
		rc := `# Normal non-login interactive Bash startup, then runner hooks.
if [[ -f "$HOME/.bashrc" ]]; then source "$HOME/.bashrc"; fi
` + common + `
# Bash continues a command list when its foreground helper is suspended.
# Restore a finished job at the next prompt (e.g. after fg), before user hooks.
unset __rr_pending_script __rr_pending_pid
__rr_restore() {
  local __rr_status=$?
  if [[ -n ${__rr_pending_script-} && -r ${__rr_pending_pid-} ]] &&
     ! builtin kill -0 "$(<"$__rr_pending_pid")" 2>/dev/null; then
    if [[ -f $__rr_pending_script ]]; then
      builtin source "$__rr_pending_script"
      __rr_status=$__rr_job_status
    fi
    ` + tmux + ` set-option -p -t "$TMUX_PANE" ` + shellJobOption + ` 0 >/dev/null 2>&1
    unset __rr_pending_script __rr_pending_pid
  fi
  return "$__rr_status"
}
__rr_restore_definition=$(declare -f __rr_restore)
__rr_busy_definition=$(declare -f __rr_busy)
__rr_ready() {
  local __rr_status=$?
  __rr_busy
  if shopt -q promptvars && [[ ${PS0-} == '$(__rr_busy)'* && -z $(jobs -pr; jobs -ps) ]] &&
     [[ $(declare -f __rr_busy) == "$__rr_busy_definition" &&
        $(declare -f __rr_restore) == "$__rr_restore_definition" &&
        ${PROMPT_COMMAND@a} != *A* && ${PROMPT_COMMAND[0]-} == __rr_restore &&
        ${PROMPT_COMMAND[-1]-} == __rr_ready ]]; then
    __rr_idle
  fi
  return "$__rr_status"
}
# Preserve scalar or indexed-array PROMPT_COMMAND, PS0, and the DEBUG trap.
if [[ ${PROMPT_COMMAND@a} != *A* ]]; then
  PS0='$(__rr_busy)'"${PS0-}"
  PROMPT_COMMAND=(__rr_restore "${PROMPT_COMMAND[@]}" __rr_ready)
fi
`
		if err = write("bashrc", rc); err != nil {
			return nil, err
		}
		args = append(args, shell, "--rcfile", filepath.Join(startup, "bashrc"), "-i")
	} else {
		restore := "unset ZDOTDIR\n"
		if hasZdotdir {
			restore = "export ZDOTDIR=" + quote(zdotdir) + "\n"
		}
		zshenv := restore + `if [[ -f ${ZDOTDIR-$HOME}/.zshenv ]]; then source "${ZDOTDIR-$HOME}/.zshenv"; fi
__rr_zdotdir_set=${+ZDOTDIR}
__rr_zdotdir=${ZDOTDIR-}
export ZDOTDIR=` + quote(startup) + "\n"
		zshrc := `if (( __rr_zdotdir_set )); then
  export ZDOTDIR=$__rr_zdotdir
else
  unset ZDOTDIR
fi
unset __rr_zdotdir_set __rr_zdotdir
if [[ -f ${ZDOTDIR-$HOME}/.zshrc ]]; then source "${ZDOTDIR-$HOME}/.zshrc"; fi
if ! zmodload zsh/parameter; then
  print -u2 -- 'remote-runner: Zsh requires its zsh/parameter module for prompt tracking'
  return 1
fi
` + common + `
# The named preexec runs BEFORE preexec_functions. Wrap it, rather than
# prepending an array hook which would leave a slow named preexec looking idle.
if (( ${+functions[preexec]} )); then
  functions[__rr_user_preexec]=$functions[preexec]
else
  unfunction __rr_user_preexec 2>/dev/null
fi
preexec() {
  __rr_busy
  if (( ${+functions[__rr_user_preexec]} )); then
    __rr_user_preexec "$@"
  fi
}
__rr_preexec_definition=$functions[preexec]
__rr_busy_definition=$functions[__rr_busy]
__rr_ready() {
  local __rr_status=$?
  __rr_busy
  local __rr_state
  for __rr_state in "${jobstates[@]}"; do
    if [[ $__rr_state == running:* || $__rr_state == suspended:* ]]; then
      return "$__rr_status"
    fi
  done
  if [[ $functions[preexec] == "$__rr_preexec_definition" &&
        $functions[__rr_busy] == "$__rr_busy_definition" &&
        ${precmd_functions[-1]-} == __rr_ready ]]; then
    __rr_idle
  fi
  return "$__rr_status"
}
precmd_functions+=(__rr_ready)
`
		if err = write(".zshenv", zshenv); err != nil {
			return nil, err
		}
		if err = write(".zshrc", zshrc); err != nil {
			return nil, err
		}
		args = append(args, "-e", "ZDOTDIR="+startup)
		args = append(args, shell, "-i")
	}
	out, err = t.output(args...)
	if err != nil {
		return nil, fmt.Errorf("%w (inspect tmux before retrying): %v", errShellLaunchUncertain, err)
	}
	ids := strings.Fields(string(out))
	if len(ids) != 2 || !shellTmuxID(ids[0], '@') || !shellTmuxID(ids[1], '%') || ids[0] == t.window {
		return nil, fmt.Errorf("%w: could not confirm window identity; inspect tmux before retrying", errShellLaunchUncertain)
	}
	window := &jobWindow{id: ids[0], pane: ids[1], shellToken: token}
	// Only this newly created, exclusively owned startup may be waited for.
	// Existing manual activity is never waited out or interrupted for admission.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err = t.shellIdle(window); err == nil {
			return window, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	status, _ := t.output("display-message", "-p", "-t", window.pane, "session=#{session_id} window=#{window_id} pane=#{pane_id} pid=#{pane_pid} shell_pid=#{"+shellPIDOption+"} state=#{"+shellStateOption+"} active=#{"+shellJobOption+"} dead=#{pane_dead} mode=#{pane_in_mode} count=#{window_panes}")
	return window, fmt.Errorf("interactive shell startup did not reach a supported idle prompt within 10s; inspect window %s and startup/prompt hooks (Bash needs promptvars and Bash 5.1+; Zsh needs normal ZDOTDIR startup); no command was sent (%s)", window.id, strings.TrimSpace(string(status)))
}

// A prompt marker is not a keyboard lock: operators must leave an untouched
// primary prompt and must not type concurrently. Unsubmitted/continuation input
// cannot be detected by these shell hooks. This guard does reject known busy,
// replaced, dead, moved, split and copy-mode panes inside tmux's command queue.
func (t terminal) shellIdleGuard(window *jobWindow) string {
	if window == nil || !shellTmuxID(window.id, '@') || !shellTmuxID(window.pane, '%') || !shellTmuxID(t.session, '$') || window.id == t.window || window.shellToken == "" || strings.Trim(window.shellToken, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") != "" {
		return "0"
	}
	conditions := []string{
		"#{==:#{session_id}," + t.session + "}",
		"#{==:#{window_id}," + window.id + "}",
		// The command targets the stable pane ID directly. Do not embed a literal
		// %N in a format comparison: display-message applies strftime expansion.
		"#{==:#{pane_dead},0}",
		"#{==:#{window_panes},1}",
		"#{==:#{pane_in_mode},0}",
		"#{==:#{" + shellJobOption + "},0}",
		"#{==:#{" + shellStateOption + "}," + window.shellToken + ":idle}",
		"#{==:#{" + shellPIDOption + "},#{pane_pid}}",
	}
	guard := "1"
	for _, condition := range conditions {
		guard = "#{&&:" + guard + "," + condition + "}"
	}
	return guard
}

func (t terminal) shellIdle(window *jobWindow) error {
	if window == nil {
		return fmt.Errorf("%w: no shell identity", ErrShellBusy)
	}
	out, err := t.output("display-message", "-p", "-t", window.pane, t.shellIdleGuard(window))
	if err != nil || strings.TrimSpace(string(out)) != "1" {
		return fmt.Errorf("%w: leave its original shell at an untouched primary prompt, outside copy mode, with no extra panes", ErrShellBusy)
	}
	return nil
}

func (t terminal) sendShellCommand(window *jobWindow, command string) error {
	if window == nil {
		return fmt.Errorf("%w: no shell identity", ErrShellBusy)
	}
	// One server-side branch checks admission, closes the idle gate, and sends
	// literal text plus Enter. A lost reply is uncertain: never retry the send.
	accepted := "set-option -p -t " + quote(window.pane) + " " + shellStateOption + " " + quote(window.shellToken+":busy") +
		" ; set-option -p -t " + quote(window.pane) + " " + shellJobOption + " 1" +
		" ; send-keys -t " + quote(window.pane) + " -l -- " + quote(command) +
		" ; send-keys -t " + quote(window.pane) + " Enter ; display-message -p remote-runner-shell-sent"
	out, err := t.output("if-shell", "-F", "-t", window.pane, t.shellIdleGuard(window), accepted, "display-message -p remote-runner-shell-busy")
	if err != nil {
		return fmt.Errorf("worktree window is busy or shell send outcome is uncertain; inspect tmux before retrying: %w", err)
	}
	if strings.TrimSpace(string(out)) == "remote-runner-shell-busy" {
		return fmt.Errorf("%w: command was not sent", ErrShellBusy)
	}
	if strings.TrimSpace(string(out)) != "remote-runner-shell-sent" {
		return fmt.Errorf("shell send outcome is uncertain; inspect tmux before retrying")
	}
	return nil
}

func shellTmuxID(value string, prefix byte) bool {
	return len(value) > 1 && value[0] == prefix && strings.Trim(value[1:], "0123456789") == ""
}
