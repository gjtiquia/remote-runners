package coordinator

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gjtiquia/remote-runners/internal/protocol"
	"github.com/gjtiquia/remote-runners/internal/repository"
)

const maxSubmissionBytes = 64 * 1024

func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func allow(w http.ResponseWriter, r *http.Request, methods ...string) bool {
	for _, method := range methods {
		if r.Method == method {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(methods, ", "))
	http.Error(w, "method not allowed", 405)
	return false
}
func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/runner" {
		if allow(w, r, "GET") {
			s.serveRunner(w, r)
		}
		return
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		http.Error(w, "management requires a loopback socket peer", 403)
		return
	}
	switch r.URL.Path {
	case "/runners":
		if !allow(w, r, "GET") {
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			http.Error(w, "coordinator closed", 503)
			return
		}
		rs := make([]protocol.RunnerInfo, 0, len(s.runners))
		for _, runner := range s.runners {
			rs = append(rs, s.runnerInfoLocked(runner))
		}
		s.mu.Unlock()
		sort.Slice(rs, func(i, j int) bool { return rs[i].ID < rs[j].ID })
		respond(w, 200, rs)
		return
	case "/jobs":
		if !allow(w, r, "GET", "POST") {
			return
		}
		if r.Method == "POST" {
			s.submit(w, r)
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			http.Error(w, "coordinator closed", 503)
			return
		}
		jobs := make([]protocol.Job, 0, len(s.order))
		for _, id := range s.order {
			jobs = append(jobs, s.jobs[id].job)
		}
		s.mu.Unlock()
		respond(w, 200, jobs)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/jobs/") {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/jobs/"), "/")
	if parts[0] == "" || len(parts) > 2 || (len(parts) == 2 && parts[1] != "output" && parts[1] != "cancel") {
		http.NotFound(w, r)
		return
	}
	method := "GET"
	if len(parts) == 2 && parts[1] == "cancel" {
		method = "POST"
	}
	if !allow(w, r, method) {
		return
	}
	if method == "POST" {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1))
		if err != nil || len(body) != 0 {
			http.Error(w, "cancellation requires an empty body", 400)
			return
		}
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		http.Error(w, "coordinator closed", 503)
		return
	}
	j := s.jobs[parts[0]]
	if j == nil {
		s.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	if len(parts) == 1 {
		copy := j.job
		s.mu.Unlock()
		respond(w, 200, copy)
		return
	}
	if parts[1] == "cancel" {
		if j.job.State == "queued" {
			s.opts.Logger.Printf("job cancellation requested job_id=%q runner_id=%q state=%q", j.job.ID, j.job.RunnerID, j.job.State)
			s.finishLocked(j, nil, protocol.Message{State: "cancelled"})
		} else if !j.job.Terminal() && !j.cancelRequested {
			j.cancelRequested = true
			s.opts.Logger.Printf("job cancellation requested job_id=%q runner_id=%q state=%q", j.job.ID, j.job.RunnerID, j.job.State)
			if runner := s.runners[j.job.RunnerID]; runner != nil {
				s.enqueueLocked(runner, protocol.Message{Type: "cancel", JobID: j.job.ID})
			}
		}
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !j.job.Terminal() {
		s.mu.Unlock()
		http.Error(w, "output is available only after completion", 409)
		return
	}
	f, err := os.Open(filepath.Join(s.dir, j.job.ID))
	s.mu.Unlock()
	if err != nil {
		http.Error(w, "output unavailable", 500)
		return
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		http.Error(w, "cannot inspect output: "+err.Error(), 500)
		return
	}
	w.Header().Set("Content-Length", fmt.Sprint(stat.Size()))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.Copy(w, f)
}
func (s *Server) submit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSubmissionBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var sub protocol.Submission
	err := decoder.Decode(&sub)
	if err == nil {
		var extra any
		err = decoder.Decode(&extra)
		if err == io.EOF {
			err = nil
		} else if err == nil {
			err = errors.New("extra JSON value")
		}
	}
	if err != nil {
		var size *http.MaxBytesError
		if errors.As(err, &size) {
			http.Error(w, "submission exceeds 64 KiB control limit", 413)
		} else {
			http.Error(w, "invalid submission JSON: "+err.Error(), 400)
		}
		return
	}
	if err = validateSubmission(sub); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	identity, err := repository.Identity(sub.Source.Remote)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if sub.Timeout == 0 {
		sub.Timeout = 30 * time.Minute
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		http.Error(w, "coordinator closed", 503)
		return
	}
	s.next++
	id := fmt.Sprintf("job-%d", s.next)
	f, err := os.OpenFile(filepath.Join(s.dir, id), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		s.mu.Unlock()
		http.Error(w, "cannot create output spool: "+err.Error(), 500)
		return
	}
	j := &record{job: protocol.Job{ID: id, Submission: sub, State: "queued", CreatedAt: s.opts.Now()}, key: protocol.Source{Remote: identity, Branch: sub.Source.Branch}, output: f}
	s.jobs[id] = j
	s.order = append(s.order, id)
	// Local identities contain literal filenames, not URL escapes. For network
	// remotes, discard authority, userinfo, query and fragment before labeling.
	projectPath := strings.TrimPrefix(identity, "file://")
	if !strings.HasPrefix(identity, "file://") {
		projectURL, err := url.Parse(identity)
		projectPath = ""
		if err == nil {
			projectPath = projectURL.Path
		}
	}
	project := strings.TrimSuffix(path.Base(projectPath), ".git")
	s.opts.Logger.Printf("job received job_id=%q branch=%q project=%q state=queued", id, sub.Source.Branch, project)
	s.dispatchLocked()
	copy := j.job
	s.mu.Unlock()
	respond(w, 201, copy)
}

// Git's check-ref-format branch rules, without requiring Git on the coordinator.
func validBranch(b string) bool {
	if b == "" || strings.HasPrefix(b, "-") || strings.Contains(b, "..") || strings.Contains(b, "@{") || strings.ContainsAny(b, "~^:?*[\\") || strings.HasSuffix(b, ".") {
		return false
	}
	for _, r := range b {
		if r <= 32 || r == 127 {
			return false
		}
	}
	for _, part := range strings.Split(b, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}
func validateSubmission(sub protocol.Submission) error {
	if strings.TrimSpace(sub.Source.Remote) == "" || strings.HasPrefix(sub.Source.Remote, "-") || strings.ContainsAny(sub.Source.Remote, "\x00\r\n") {
		return errors.New("source remote is required and must not contain NUL or newlines")
	}
	b := sub.Source.Branch
	if !validBranch(b) {
		return errors.New("source branch must be a valid Git branch name")
	}
	if len(sub.Args) == 0 || sub.Args[0] == "" {
		return errors.New("executable is required")
	}
	for _, arg := range sub.Args {
		if strings.ContainsRune(arg, 0) {
			return errors.New("arguments must not contain NUL")
		}
	}
	if sub.Timeout < 0 {
		return errors.New("timeout must not be negative")
	}
	return nil
}
