// Package coordinator owns the in-memory registry, queue, and job history.
package coordinator

import (
	"errors"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gjtiquia/remote-runners/internal/protocol"
	"github.com/gorilla/websocket"
)

// Options selects lifetime-owned output storage and heartbeat policy. Zero
// heartbeat values use defaults. Now must be safe for concurrent calls.
type Options struct {
	OutputDir         string
	HeartbeatInterval time.Duration
	MissLimit         int
	Now               func() time.Time
}

type record struct {
	job             protocol.Job
	key             protocol.Source
	output          *os.File
	outputError     error
	outputReceived  bool
	cancelRequested bool
}
type registration struct {
	info         protocol.RunnerInfo
	id           string
	conn         *websocket.Conn
	active       map[string]bool
	reported     map[string]bool // assignments known to occupy the last reported capacity
	outbox       []protocol.Message
	wake         chan struct{}
	done         chan struct{}
	stopped      bool
	sequence     uint64
	lastResponse uint64
	awaiting     bool
}

// Server keeps registry, queue, branch locks, and history for one lifetime.
type Server struct {
	mu          sync.Mutex
	opts        Options
	dir         string
	jobs        map[string]*record
	order       []string
	runners     map[string]*registration
	locks       map[protocol.Source]string
	connections map[*websocket.Conn]bool
	next        uint64
	closed      bool
	stop        chan struct{}
	lastTick    time.Time
	wg          sync.WaitGroup
	closeOnce   sync.Once
	closeErr    error
}

// New creates a fresh owned spool and starts the heartbeat ticker.
func New(opts Options) (*Server, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.HeartbeatInterval == 0 {
		opts.HeartbeatInterval = 10 * time.Second
	}
	if opts.MissLimit == 0 {
		opts.MissLimit = 3
	}
	if opts.HeartbeatInterval < 0 || opts.MissLimit < 1 {
		return nil, errors.New("invalid heartbeat policy")
	}
	if opts.OutputDir != "" {
		if err := os.MkdirAll(opts.OutputDir, 0700); err != nil {
			return nil, err
		}
	}
	dir, err := os.MkdirTemp(opts.OutputDir, "remote-runners-")
	if err != nil {
		return nil, err
	}
	s := &Server{opts: opts, dir: dir, jobs: make(map[string]*record), runners: make(map[string]*registration), locks: make(map[protocol.Source]string), connections: make(map[*websocket.Conn]bool), stop: make(chan struct{}), lastTick: opts.Now()}
	s.wg.Add(1)
	go s.heartbeats()
	return s, nil
}
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }

// Close stops coordinator connections and cleans its spool, not remote jobs.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		close(s.stop)
		for _, r := range s.runners {
			s.stopTransportLocked(r)
		}
		for c := range s.connections {
			_ = c.Close()
		}
		s.mu.Unlock()
		s.wg.Wait()
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, j := range s.jobs {
			if j.output != nil {
				s.closeErr = errors.Join(s.closeErr, j.output.Close())
				j.output = nil
			}
		}
		s.closeErr = errors.Join(s.closeErr, os.RemoveAll(s.dir))
	})
	return s.closeErr
}
func (s *Server) runnerInfoLocked(r *registration) protocol.RunnerInfo {
	info := r.info
	info.ActiveJobs = len(r.active)
	info.Slots = s.availableSlotsLocked(r)
	info.Available = s.eligibleLocked(r)
	return info
}
func (s *Server) eligibleLocked(r *registration) bool {
	return !r.stopped && r.info.Misses == 0 && hasReportedCapacity(r.info) && s.availableSlotsLocked(r) > 0
}

func (s *Server) dispatchLocked() {
	for _, id := range s.order {
		j := s.jobs[id]
		if j.job.State != "queued" || s.locks[j.key] != "" {
			continue
		}
		var chosen *registration
		for _, r := range s.runners {
			if !s.eligibleLocked(r) {
				continue
			}
			// Equal-priority ties use ascending runner ID, independent of registration order.
			if chosen == nil || r.info.Priority > chosen.info.Priority || (r.info.Priority == chosen.info.Priority && r.info.ID < chosen.info.ID) {
				chosen = r
			}
		}
		if chosen == nil {
			return
		}
		j.job.State = "running"
		j.job.RunnerID = chosen.info.ID
		j.job.StartedAt = s.opts.Now()
		chosen.active[id] = true
		s.locks[j.key] = id
		copy := j.job
		s.enqueueLocked(chosen, protocol.Message{Type: "job", Job: &copy})
	}
}

// Subtract assignments not yet reflected in the runner's last capacity report.
func (s *Server) availableSlotsLocked(r *registration) int {
	unreported := len(r.active) - r.info.ActiveJobs
	if unreported < 0 {
		unreported = 0
	}
	slots := min(r.info.Slots-unreported, r.info.MaxJobs-len(r.active))
	if slots < 0 {
		return 0
	}
	return slots
}
func (s *Server) finishLocked(j *record, r *registration, m protocol.Message) {
	j.job.State = m.State
	j.job.ExitCode = m.ExitCode
	j.job.Error = m.Error
	j.job.Commit = m.Commit
	j.job.FinishedAt = s.opts.Now()
	if j.output != nil {
		if err := j.output.Close(); err != nil {
			j.outputError = errors.Join(j.outputError, err)
		}
		j.output = nil
	}
	if j.outputError != nil {
		j.job.State = "failed"
		j.job.ExitCode = nil
		j.job.Error = "output spool failed: " + j.outputError.Error()
		if m.Error != "" {
			j.job.Error += "; runner result: " + m.Error
		}
	}
	if r != nil {
		delete(r.active, j.job.ID)
	}
	if s.locks[j.key] == j.job.ID {
		delete(s.locks, j.key)
	}
	s.dispatchLocked()
}
