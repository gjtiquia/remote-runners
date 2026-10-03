package coordinator

import (
	"crypto/rand"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gjtiquia/remote-runners/internal/protocol"
	"github.com/gorilla/websocket"
)

// Output arrives in bounded messages, while total job output has no size cap.
const MaxOutputChunk = 64 * 1024
const maxControlMessage = 128 * 1024
const writeTimeout = 10 * time.Second

func (s *Server) serveRunner(w http.ResponseWriter, req *http.Request) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	conn, err := upgrader.Upgrade(w, req, nil)
	if err != nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		conn.Close()
		return
	}
	s.connections[conn] = true
	s.wg.Add(1)
	s.mu.Unlock()
	defer func() { conn.Close(); s.mu.Lock(); delete(s.connections, conn); s.mu.Unlock(); s.wg.Done() }()
	conn.SetReadLimit(maxControlMessage)
	_ = conn.SetReadDeadline(time.Now().Add(writeTimeout))
	var m protocol.Message
	if err := conn.ReadJSON(&m); err != nil || m.Type != "register" || m.Runner == nil || !validRunner(*m.Runner) {
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if previous := s.runners[m.Runner.ID]; previous != nil {
		s.deregisterLocked(previous, "runner replaced by manual registration; stop old remote job windows before restarting")
	}
	s.next++
	r := &registration{info: *m.Runner, id: "registration-" + rand.Text(), conn: conn, active: make(map[string]bool), wake: make(chan struct{}, 1), done: make(chan struct{})}
	r.info.Misses = 0 // Timing policy belongs exclusively to the coordinator.
	s.runners[r.info.ID] = r
	s.enqueueLocked(r, protocol.Message{Type: "registered", RegistrationID: r.id})
	s.wg.Add(1)
	go s.writeRunner(r)
	s.dispatchLocked()
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.stopTransportLocked(r); s.mu.Unlock() }()
	for {
		var msg protocol.Message
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}
		s.mu.Lock()
		if s.closed || s.runners[r.info.ID] != r || msg.RegistrationID != r.id {
			s.mu.Unlock()
			continue
		}
		if msg.Type == "heartbeat_response" && msg.Sequence > r.lastResponse && msg.Sequence <= r.sequence && msg.Runner != nil && validRunner(*msg.Runner) && msg.Runner.ID == r.info.ID {
			r.lastResponse = msg.Sequence
			s.reportCapacityLocked(r, *msg.Runner, "")
			r.info.Misses = 0
			r.awaiting = false
			s.dispatchLocked()
		}
		j := s.jobs[msg.JobID]
		if j != nil && r.active[msg.JobID] && j.job.State == "running" {
			switch msg.Type {
			case "output":
				j.outputReceived = true
				if len(msg.Data) > MaxOutputChunk {
					s.mu.Unlock()
					return
				}
				if j.output != nil && j.outputError == nil {
					n, err := j.output.Write(msg.Data)
					if err == nil && n != len(msg.Data) {
						err = io.ErrShortWrite
					}
					if err != nil {
						// Infrastructure failure requests cancellation but must not free the
						// branch or execution slot while the remote process is still active.
						j.outputError = err
						j.job.Error = "output spool failed: " + err.Error()
						if !j.cancelRequested {
							j.cancelRequested = true
							s.enqueueLocked(r, protocol.Message{Type: "cancel", JobID: j.job.ID})
						}
					}
				}
			case "decline":
				// Only an unstarted assignment may be returned, with a valid
				// unavailable snapshot. Never immediately redispatch to its sender.
				if !j.outputReceived && msg.Runner != nil && validRunner(*msg.Runner) && msg.Runner.ID == r.info.ID && !hasReportedCapacity(*msg.Runner) {
					s.reportCapacityLocked(r, *msg.Runner, j.job.ID)
					delete(r.active, j.job.ID)
					delete(s.locks, j.key)
					j.job.State = "queued"
					j.job.RunnerID = ""
					j.job.StartedAt = time.Time{}
					if j.cancelRequested {
						s.finishLocked(j, nil, protocol.Message{State: "cancelled"})
					} else {
						s.dispatchLocked()
					}
				}
			case "complete":
				if (protocol.Job{State: msg.State}).Terminal() {
					if msg.Runner != nil && validRunner(*msg.Runner) && msg.Runner.ID == r.info.ID {
						s.reportCapacityLocked(r, *msg.Runner, j.job.ID)
					} else if msg.Runner == nil && r.reported[j.job.ID] {
						// Legacy senders omit completion telemetry. Reclaim only a
						// slot known to include this job; never infer memory recovery.
						delete(r.reported, j.job.ID)
						r.info.ActiveJobs--
						r.info.Slots = min(r.info.Slots+1, r.info.MaxJobs-r.info.ActiveJobs)
					}
					s.finishLocked(j, r, msg)
				}
			}
		}
		s.mu.Unlock()
	}
}

// A report on decline/completion excludes that job. When a snapshot accounts
// for every other assignment, their occupancy is known; partial snapshots are
// ambiguous and must not grant extra capacity on a later legacy completion.
func (s *Server) reportCapacityLocked(r *registration, info protocol.RunnerInfo, excluded string) {
	misses := r.info.Misses
	r.info = info
	r.info.Misses = misses
	r.reported = make(map[string]bool)
	count := len(r.active)
	if r.active[excluded] {
		count--
	}
	if info.ActiveJobs == count {
		for id := range r.active {
			if id != excluded {
				r.reported[id] = true
			}
		}
	}
}

func hasReportedCapacity(r protocol.RunnerInfo) bool {
	return r.Slots > 0 && r.ActiveJobs < r.MaxJobs && r.AvailableMemory >= r.MinMemory
}

func validRunner(r protocol.RunnerInfo) bool {
	return strings.TrimSpace(r.ID) != "" && len(r.ID) <= 128 && len(r.Name) <= 256 && !strings.ContainsAny(r.ID, "\x00\r\n") && r.Priority >= 0 && r.Priority <= 100 && r.MaxJobs > 0 && r.Slots >= 0 && r.ActiveJobs >= 0
}
func (s *Server) enqueueLocked(r *registration, m protocol.Message) {
	if r.stopped {
		return
	}
	if m.RegistrationID == "" {
		m.RegistrationID = r.id
	}
	r.outbox = append(r.outbox, m)
	select {
	case r.wake <- struct{}{}:
	default:
	}
}
func (s *Server) stopTransportLocked(r *registration) {
	if r.stopped {
		return
	}
	r.stopped = true
	close(r.done)
	r.outbox = nil
	_ = r.conn.Close()
}

// A single writer per connection owns writes and write deadlines. State changes
// enqueue under the mutex; network writes never hold the global state mutex.
func (s *Server) writeRunner(r *registration) {
	defer s.wg.Done()
	for {
		select {
		case <-r.done:
			return
		case <-r.wake:
		}
		for {
			s.mu.Lock()
			if r.stopped || len(r.outbox) == 0 {
				s.mu.Unlock()
				break
			}
			m := r.outbox[0]
			r.outbox[0] = protocol.Message{}
			r.outbox = r.outbox[1:]
			s.mu.Unlock()
			_ = r.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			if err := r.conn.WriteJSON(m); err != nil {
				s.mu.Lock()
				s.stopTransportLocked(r)
				s.mu.Unlock()
				return
			}
		}
	}
}
