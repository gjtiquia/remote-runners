// Package runner implements the outbound tmux worker and its private job helper.
package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Config is the editable machine configuration. Memory is expressed in bytes.
type Config struct {
	CoordinatorURL string `json:"coordinator_url"`
	Name           string `json:"name"`
	Priority       int    `json:"priority"`
	MaxJobs        int    `json:"max_jobs"`
	MinMemory      uint64 `json:"min_memory"`
	MaxWindows     int    `json:"max_windows"`
	Repos          string `json:"repos"`
	Worktrees      string `json:"worktrees"`
}

func loadDefaultConfig(home string) (Config, error) {
	path := filepath.Join(home, ".remote-runners", "config.json")
	if _, err := os.Lstat(path); err == nil {
		return LoadConfig(path) // An existing destination always wins.
	} else if !os.IsNotExist(err) {
		return Config{}, err
	}
	legacy := filepath.Join(home, ".remote-runner", "config.json")
	info, err := os.Lstat(legacy)
	if os.IsNotExist(err) {
		return LoadConfig(path)
	}
	if err != nil {
		return Config{}, err
	}
	if !info.Mode().IsRegular() {
		return Config{}, fmt.Errorf("legacy configuration %s is not a regular file; move its contents to %s manually", legacy, path)
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return Config{}, err
	}
	// Link then unlink: publish the existing bytes atomically, without replacing
	// a destination created concurrently. Linux/macOS support this on one FS.
	if err = os.Link(legacy, path); err != nil {
		if os.IsExist(err) {
			return LoadConfig(path)
		}
		return Config{}, fmt.Errorf("move configuration from %s to %s manually: %w", legacy, path, err)
	}
	if err = os.Remove(legacy); err != nil {
		return Config{}, fmt.Errorf("configuration preserved at %s, but remove legacy file %s manually: %w", path, legacy, err)
	}
	progressLogger("remote-runner").Printf("configuration moved from %q to %q", legacy, path)
	return LoadConfig(path)
}

// LoadConfig initializes missing/empty configuration, then requires deliberate setup.
func LoadConfig(path string) (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, err
	}
	defaults := Config{Priority: 50, MaxJobs: 5, MinMemory: 1 << 30, MaxWindows: 8, Repos: filepath.Join(home, ".remote-runners", "repos"), Worktrees: filepath.Join(home, ".remote-runners", "worktrees")}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) || (err == nil && len(bytes.TrimSpace(data)) == 0) {
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return Config{}, err
		}
		data, _ = json.MarshalIndent(defaults, "", "  ")
		if err = os.WriteFile(path, append(data, '\n'), 0600); err != nil {
			return Config{}, err
		}
		return defaults, fmt.Errorf("starter configuration created: edit %s and set coordinator_url and name", path)
	}
	if err != nil {
		return Config{}, err
	}
	if err = json.Unmarshal(data, &defaults); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if err = defaults.validate(); err != nil {
		return defaults, fmt.Errorf("edit %s: %w", path, err)
	}
	return defaults, nil
}
func (c Config) validate() error {
	if strings.TrimSpace(c.CoordinatorURL) == "" || strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("coordinator_url and name are required")
	}
	endpoint, err := url.Parse(c.CoordinatorURL)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "ws" && endpoint.Scheme != "wss" && endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return fmt.Errorf("coordinator_url must be an absolute ws/wss or http/https URL, e.g. ws://host:2461/runner")
	}
	if c.Priority < 0 || c.Priority > 100 || c.MaxJobs < 1 || c.MaxWindows < 1 || c.MinMemory == 0 || c.Repos == "" || c.Worktrees == "" {
		return fmt.Errorf("require priority 0–100, positive max_jobs/max_windows/min_memory, and repos/worktrees")
	}
	return nil
}
