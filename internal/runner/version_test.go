package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gjtiquia/remote-runners/internal/buildinfo"
)

func TestWorkerSurfacesCoordinatorVersionRejection(t *testing.T) {
	const reason = "version rejected: worker build is dirty; commit changes, run go install ./cmd/... and restart"
	reported := make(chan buildinfo.Info, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reported <- buildinfo.FromHeaders(r.Header)
		http.Error(w, reason, http.StatusPreconditionFailed)
	}))
	defer server.Close()
	conn, err := dialCoordinator(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"))
	if conn != nil {
		conn.Close()
		t.Fatal("rejected worker connected")
	}
	if err == nil || !strings.Contains(err.Error(), reason) || !strings.Contains(err.Error(), "412") {
		t.Fatalf("lost coordinator rejection: %v", err)
	}
	// Even unknown test-binary identity is reported to the coordinator, not
	// rejected locally; the returned error reaches Main's stderr logging.
	got := <-reported
	own := buildinfo.Current()
	if own.Known && got != own || !own.Known && got.Known {
		t.Fatalf("reported %+v, binary %+v", got, own)
	}
}
