package coordinator

import (
	"fmt"
	"net/http"

	"github.com/gjtiquia/remote-runners/internal/buildinfo"
)

const rebuildInstruction = "commit or stash local changes, then run go install ./cmd/... from a clean Git checkout with VCS metadata enabled, and restart the installed processes"

func validBuild(role string, info buildinfo.Info) error {
	if !info.Known || !buildinfo.ValidRevision(info.Revision) {
		return fmt.Errorf("%s build identity is unknown (missing or invalid Git revision/dirty metadata); %s", role, rebuildInstruction)
	}
	if info.Modified {
		return fmt.Errorf("%s build is dirty (revision %s contains build-time local modifications); %s", role, info.Revision, rebuildInstruction)
	}
	return nil
}

// acceptBuild is the single policy seam for management and worker admission.
// Peers only report identity; the coordinator alone decides compatibility.
func (s *Server) acceptBuild(w http.ResponseWriter, r *http.Request, role string) bool {
	peer := buildinfo.FromHeaders(r.Header)
	err := validBuild(role, peer)
	if err == nil && peer.Revision != s.build.Revision {
		err = fmt.Errorf("%s/coordinator version mismatch: %s revision %s, coordinator revision %s; install all executables from a clean checkout of coordinator revision %s using go install ./cmd/... and restart them", role, role, peer.Revision, s.build.Revision, s.build.Revision)
	}
	if err == nil {
		return true
	}
	message := fmt.Sprintf("version rejected: %v (coordinator revision %s)", err, s.build.Revision)
	s.opts.Logger.Printf("%s peer=%q role=%q", message, r.RemoteAddr, role)
	http.Error(w, message, http.StatusPreconditionFailed)
	return false
}
