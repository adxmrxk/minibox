// Package jobs holds the job types shared by the coordinator (mbd) and the agents (mbagent).
package jobs

import (
	"errors"
	"strings"
	"time"
)

// Status is where a job is in its life.
type Status string

const (
	Queued    Status = "queued"
	Assigned  Status = "assigned"
	Succeeded Status = "succeeded"
	Failed    Status = "failed"
	TimedOut  Status = "timed_out"
)

// Finished reports whether a status is terminal.
func (s Status) Finished() bool {
	return s == Succeeded || s == Failed || s == TimedOut
}

// Spec describes what to run. It maps onto the flags of `minibox run`.
type Spec struct {
	Image       string            `json:"image"`
	Cmd         []string          `json:"cmd"`
	MemMB       int               `json:"mem_mb,omitempty"`
	CPUs        float64           `json:"cpus,omitempty"`
	Pids        int               `json:"pids,omitempty"`
	TimeoutSecs int               `json:"timeout_secs,omitempty"`
	Net         string            `json:"net,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Workdir     string            `json:"workdir,omitempty"`
}

// Validate rejects specs that would not run safely or at all.
func (s Spec) Validate() error {
	if strings.TrimSpace(s.Image) == "" {
		// Without an image the job would share the host filesystem.
		return errors.New("image is required")
	}
	if len(s.Cmd) == 0 {
		return errors.New("cmd is required")
	}
	switch s.Net {
	case "", "isolated", "bridge", "host":
	default:
		return errors.New("net must be isolated, bridge or host")
	}
	if s.MemMB < 0 || s.Pids < 0 || s.TimeoutSecs < 0 || s.CPUs < 0 {
		return errors.New("limits cannot be negative")
	}
	if s.Workdir != "" && !strings.HasPrefix(s.Workdir, "/") {
		return errors.New("workdir must be an absolute path")
	}
	return nil
}

// Job is one unit of work and its result.
type Job struct {
	ID        string    `json:"id"`
	Spec      Spec      `json:"spec"`
	Status    Status    `json:"status"`
	Agent     string    `json:"agent,omitempty"`
	ExitCode  *int      `json:"exit_code,omitempty"`
	Log       string    `json:"log,omitempty"`
	Submitted time.Time `json:"submitted"`
	Updated   time.Time `json:"updated"`
}
