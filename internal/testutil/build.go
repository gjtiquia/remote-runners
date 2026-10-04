package testutil

import (
	"net/http"
	"os/exec"
	"strings"

	"github.com/gjtiquia/remote-runners/internal/buildinfo"
	"github.com/gjtiquia/remote-runners/internal/client"
)

// Build returns the clean build identity for binaries built from this checkout.
func Build() buildinfo.Info {
	revision, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		panic(err)
	}
	return buildinfo.Info{Revision: strings.TrimSpace(string(revision)), Known: true}
}

// PtrBuild returns a pointer to this checkout's build identity.
func PtrBuild() *buildinfo.Info {
	build := Build()
	return &build
}

// Client creates an API client advertising this checkout's build identity.
func Client(url string) *client.Client {
	c := client.New(url)
	c.Build = Build()
	return c
}

// Headers returns HTTP headers advertising this checkout's build identity.
func Headers() http.Header {
	headers := make(http.Header)
	buildinfo.SetHeaders(headers, Build())
	return headers
}
