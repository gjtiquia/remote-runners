package runner

import (
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func progressLogger(component string) *log.Logger {
	return log.New(os.Stderr, component+" ", log.LstdFlags)
}

// Project labels reveal only the repository basename, never URL authority,
// credentials, query parameters, or the full remote. Callers quote the label.
func projectLabel(remote string) string {
	if strings.Contains(remote, "://") {
		u, err := url.Parse(remote)
		if err != nil {
			return "unknown"
		}
		remote = u.Path
	} else if i := strings.IndexByte(remote, ':'); i > 0 && !strings.ContainsAny(remote[:i], "/\\") {
		remote = remote[i+1:]
	}
	return strings.TrimSuffix(filepath.Base(remote), ".git")
}

func exitLabel(code *int) string {
	if code == nil {
		return "unknown"
	}
	return strconv.Itoa(*code)
}

func (w *worker) progress(format string, args ...any) {
	w.logger.Printf("runner=%q "+format, append([]any{w.cfg.Name}, args...)...)
}
