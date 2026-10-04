package coordinator_test

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gjtiquia/remote-runners/internal/buildinfo"
	"github.com/gjtiquia/remote-runners/internal/client"
	"github.com/gjtiquia/remote-runners/internal/coordinator"
	"github.com/gorilla/websocket"
)

func cleanBuild() buildinfo.Info {
	return buildinfo.Info{Revision: strings.Repeat("a", 40), Known: true}
}

func TestCoordinatorRejectsInvalidOwnBuild(t *testing.T) {
	for _, info := range []buildinfo.Info{{}, {Revision: strings.Repeat("a", 40), Known: true, Modified: true}} {
		var logs bytes.Buffer
		s, err := coordinator.New(coordinator.Options{Build: &info, Logger: log.New(&logs, "", 0), OutputDir: t.TempDir()})
		if s != nil || err == nil {
			t.Fatal("invalid coordinator started")
		}
		if !strings.Contains(err.Error(), "go install ./cmd/...") || !strings.Contains(logs.String(), err.Error()) {
			t.Fatalf("missing actionable error/log: %v %s", err, &logs)
		}
	}
}

func TestCoordinatorVersionAdmission(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build buildinfo.Info
		want  string
	}{
		{"unknown", buildinfo.Info{}, "unknown"},
		{"dirty", buildinfo.Info{Revision: strings.Repeat("a", 40), Known: true, Modified: true}, "dirty"},
		{"mismatch", buildinfo.Info{Revision: strings.Repeat("b", 40), Known: true}, "version mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			own := cleanBuild()
			s, err := coordinator.New(coordinator.Options{Build: &own, Logger: log.New(&logs, "", 0), OutputDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			// Direct requests allow deterministic assertions on both response and log.
			for _, route := range []struct{ method, path string }{{"POST", "/jobs"}, {"GET", "/jobs"}, {"GET", "/runners"}, {"GET", "/jobs/missing"}, {"GET", "/jobs/missing/output"}, {"POST", "/jobs/missing/cancel"}, {"GET", "/runner"}} {
				req := httptest.NewRequest(route.method, route.path, strings.NewReader(`{}`))
				req.RemoteAddr = "127.0.0.1:1234"
				buildinfo.SetHeaders(req.Header, tc.build)
				rec := httptest.NewRecorder()
				s.Handler().ServeHTTP(rec, req)
				body := strings.TrimSpace(rec.Body.String())
				if rec.Code != http.StatusPreconditionFailed || !strings.Contains(body, tc.want) || !strings.Contains(body, "go install ./cmd/...") || !strings.Contains(body, own.Revision) {
					t.Fatalf("%s %s: %d %s", route.method, route.path, rec.Code, body)
				}
				if !strings.Contains(logs.String(), body) {
					t.Fatalf("rejection not logged: %s", &logs)
				}
			}
			server := httptest.NewServer(s.Handler())
			defer server.Close()
			api := client.New(server.URL)
			api.Build = tc.build
			if _, err := api.Runners(context.Background()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("thin client did not surface rejection: %v", err)
			}
			headers := make(http.Header)
			buildinfo.SetHeaders(headers, tc.build)
			conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/runner", headers)
			if conn != nil {
				conn.Close()
				t.Fatal("rejected worker upgraded")
			}
			if err == nil || response == nil || response.StatusCode != http.StatusPreconditionFailed {
				t.Fatalf("worker rejection: %v %v", response, err)
			}
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if !strings.Contains(string(body), tc.want) {
				t.Fatalf("worker rejection lost reason: %s", body)
			}
			api.Build = own
			jobs, err := api.Jobs(context.Background())
			if err != nil || len(jobs) != 0 {
				t.Fatalf("rejection accepted work: %+v %v", jobs, err)
			}
			runners, err := api.Runners(context.Background())
			if err != nil || len(runners) != 0 {
				t.Fatalf("rejection registered runner: %+v %v", runners, err)
			}
		})
	}
}

func TestBuildHeadersDoNotAuthorizeManagement(t *testing.T) {
	own := cleanBuild()
	s, err := coordinator.New(coordinator.Options{Build: &own, OutputDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	req := httptest.NewRequest("GET", "/runners", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	buildinfo.SetHeaders(req.Header, own)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("version identity authorized management: %d", rec.Code)
	}
}
