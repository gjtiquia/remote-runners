// Package buildinfo reports identity baked into a binary by Go's VCS stamping.
// It reports facts only; compatibility policy belongs to the coordinator.
package buildinfo

import (
	"net/http"
	"runtime/debug"
	"strconv"
)

const (
	RevisionHeader = "X-Remote-Runners-Revision"
	ModifiedHeader = "X-Remote-Runners-Modified"
)

type Info struct {
	Revision string
	Modified bool
	Known    bool
}

// Current reads build-time metadata, never the current working directory.
func Current() Info {
	b, ok := debug.ReadBuildInfo()
	if !ok {
		return Info{}
	}
	return fromBuildInfo(b)
}

func fromBuildInfo(b *debug.BuildInfo) Info {
	var info Info
	var git, modifiedKnown bool
	for _, s := range b.Settings {
		switch s.Key {
		case "vcs":
			git = s.Value == "git"
		case "vcs.revision":
			info.Revision = s.Value
		case "vcs.modified":
			var err error
			info.Modified, err = strconv.ParseBool(s.Value)
			modifiedKnown = err == nil
		}
	}
	info.Known = git && modifiedKnown && ValidRevision(info.Revision)
	return info
}

func ValidRevision(revision string) bool {
	if len(revision) != 40 && len(revision) != 64 {
		return false
	}
	for _, c := range revision {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// SetHeaders reports identity without making any compatibility decision.
func SetHeaders(h http.Header, info Info) {
	h.Del(RevisionHeader)
	h.Del(ModifiedHeader)
	if !info.Known {
		return
	}
	h.Set(RevisionHeader, info.Revision)
	h.Set(ModifiedHeader, strconv.FormatBool(info.Modified))
}

func FromHeaders(h http.Header) Info {
	revisions, modified := h.Values(RevisionHeader), h.Values(ModifiedHeader)
	if len(revisions) != 1 || len(modified) != 1 {
		return Info{}
	}
	info := Info{Revision: revisions[0]}
	if modified[0] != "true" && modified[0] != "false" {
		return info
	}
	info.Modified = modified[0] == "true"
	info.Known = ValidRevision(info.Revision)
	return info
}
