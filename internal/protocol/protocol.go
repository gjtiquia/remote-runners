// Package protocol defines the coordinator's public wire contract.
package protocol

import "time"

type Source struct {
	Remote string `json:"remote"`
	Branch string `json:"branch"`
}
type Submission struct {
	Source  Source        `json:"source"`
	Args    []string      `json:"args"`
	Timeout time.Duration `json:"timeout"`
}
type Job struct {
	ID string `json:"id"`
	Submission
	State      string    `json:"state"`
	RunnerID   string    `json:"runner_id,omitempty"`
	ExitCode   *int      `json:"exit_code,omitempty"`
	Error      string    `json:"error,omitempty"`
	Commit     string    `json:"commit,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

func (j Job) Terminal() bool {
	return j.State == "succeeded" || j.State == "failed" || j.State == "cancelled" || j.State == "timed_out"
}

type RunnerInfo struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Priority        int    `json:"priority"`
	MaxJobs         int    `json:"max_jobs"`
	MinMemory       uint64 `json:"min_memory"`
	AvailableMemory uint64 `json:"available_memory"`
	// Slots is currently available local execution/window capacity.
	Slots      int  `json:"slots"`
	ActiveJobs int  `json:"active_jobs"`
	Available  bool `json:"available"`
	Misses     int  `json:"misses"`
}
type Message struct {
	Type           string      `json:"type"`
	Runner         *RunnerInfo `json:"runner,omitempty"`
	Job            *Job        `json:"job,omitempty"`
	JobID          string      `json:"job_id,omitempty"`
	RegistrationID string      `json:"registration_id,omitempty"`
	Sequence       uint64      `json:"sequence,omitempty"`
	Data           []byte      `json:"data,omitempty"`
	State          string      `json:"state,omitempty"`
	ExitCode       *int        `json:"exit_code,omitempty"`
	Error          string      `json:"error,omitempty"`
	Commit         string      `json:"commit,omitempty"`
}
