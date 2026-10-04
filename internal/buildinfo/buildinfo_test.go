package buildinfo

import (
	"net/http"
	"runtime/debug"
	"strings"
	"testing"
)

func TestBuildMetadata(t *testing.T) {
	revision := strings.Repeat("a", 40)
	for _, tc := range []struct {
		name, vcs, sha, modified string
		known, dirty             bool
	}{
		{"clean", "git", revision, "false", true, false},
		{"dirty", "git", revision, "true", true, true},
		{"missing dirty state", "git", revision, "", false, false},
		{"invalid dirty state", "git", revision, "maybe", false, false},
		{"missing revision", "git", "", "false", false, false},
		{"malformed revision", "git", "HEAD", "false", false, false},
		{"non git", "hg", revision, "false", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs", Value: tc.vcs}, {Key: "vcs.revision", Value: tc.sha}, {Key: "vcs.modified", Value: tc.modified}}}
			got := fromBuildInfo(b)
			if got.Known != tc.known || got.Modified != tc.dirty {
				t.Fatalf("got %+v", got)
			}
		})
	}
	if got := fromBuildInfo(&debug.BuildInfo{}); got.Known {
		t.Fatal("empty metadata is known")
	}
}

func TestHeaders(t *testing.T) {
	info := Info{Revision: strings.Repeat("b", 40), Known: true, Modified: true}
	h := make(http.Header)
	SetHeaders(h, info)
	if got := FromHeaders(h); got != info {
		t.Fatalf("round trip: %+v", got)
	}
	h.Add(RevisionHeader, info.Revision)
	if FromHeaders(h).Known {
		t.Fatal("duplicate revision accepted")
	}
	SetHeaders(h, info)
	h.Add(ModifiedHeader, "false")
	if FromHeaders(h).Known {
		t.Fatal("duplicate dirty state accepted")
	}
	SetHeaders(h, info)
	h.Set(ModifiedHeader, "TRUE")
	if FromHeaders(h).Known {
		t.Fatal("malformed dirty state accepted")
	}
	SetHeaders(h, Info{})
	if FromHeaders(h).Known || h.Get(RevisionHeader) != "" {
		t.Fatal("unknown identity retained headers")
	}
}
