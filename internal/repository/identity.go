// Package repository defines shared logical repository identity.
package repository

import (
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"strings"
)

// Match worker repository identity: URL/scp SSH aliases, host case, default SSH
// port, and local file URLs normalize; different transports/users remain distinct.
// Keep the original remote in assignments so Git sees the operator's spelling.
// Identity is shared by scheduling, workspace allocation, and tmux window reuse.
func Identity(remote string) (string, error) {
	if strings.Contains(remote, "://") {
		u, err := url.Parse(remote)
		if err != nil {
			return "", fmt.Errorf("invalid remote URL: %w", err)
		}
		if u.Scheme == "file" {
			if u.Host != "" && u.Host != "localhost" {
				return "", fmt.Errorf("file remote must be local")
			}
			remote = u.Path
		} else {
			u.Scheme = strings.ToLower(u.Scheme)
			u.Host = strings.ToLower(u.Host)
			if u.Scheme == "ssh" && u.Port() == "22" {
				u.Host = u.Hostname()
				if strings.Contains(u.Host, ":") {
					u.Host = "[" + u.Host + "]"
				}
			}
			if u.Scheme == "ssh" {
				u.Path = path.Clean(u.Path)
			}
			return u.String(), nil
		}
	} else if i := strings.IndexByte(remote, ':'); i > 0 && !strings.ContainsAny(remote[:i], "/\\") {
		authority := remote[:i]
		remotePath := path.Clean(remote[i+1:])
		// scp relative paths resolve in the SSH account's home, unlike /absolute URL paths.
		if !strings.HasPrefix(remotePath, "/") {
			remotePath = "/~/" + remotePath
		}
		u := &url.URL{Scheme: "ssh", Path: remotePath}
		if at := strings.LastIndexByte(authority, '@'); at >= 0 {
			u.User = url.User(authority[:at])
			authority = authority[at+1:]
		}
		u.Host = strings.ToLower(authority)
		return u.String(), nil
	}
	absolute, err := filepath.Abs(remote)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		absolute = resolved
	}
	return "file://" + filepath.ToSlash(absolute), nil
}
