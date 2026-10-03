package coordinator

import (
	"log"
	"time"

	"github.com/gjtiquia/remote-runners/internal/protocol"
)

func (s *Server) deregisterLocked(r *registration, reason string) {
	delete(s.runners, r.info.ID)
	s.stopTransportLocked(r)
	log.Printf("runner %q registration %q deregistered: %s", r.info.ID, r.id, reason)
	for id := range r.active {
		s.finishLocked(s.jobs[id], r, protocol.Message{State: "failed", Error: reason})
	}
}

func (s *Server) heartbeats() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.opts.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.Tick(s.opts.Now())
		}
	}
}

// Tick observes one heartbeat round if the configured interval has elapsed.
// Tests can supply Now and call Tick explicitly; the normal ticker stays enabled.
func (s *Server) Tick(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || now.Sub(s.lastTick) < s.opts.HeartbeatInterval {
		return
	}
	s.lastTick = now
	for _, r := range s.runners {
		if r.awaiting {
			r.info.Misses++
		}
	}
	// Make every runner's eligibility current before releasing any branch locks.
	// Deregistration can dispatch queued work through finishLocked.
	for _, r := range s.runners {
		if r.info.Misses >= s.opts.MissLimit {
			s.deregisterLocked(r, "runner missed heartbeat cutoff")
			continue
		}
		r.sequence++
		r.awaiting = true
		s.enqueueLocked(r, protocol.Message{Type: "heartbeat", Sequence: r.sequence})
	}
}
