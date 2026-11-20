// mbagent runs on a host. It polls the coordinator (mbd) for jobs, runs each one
// with the minibox binary, and reports the result back.
//
//	mbagent --server http://coordinator:8787 --token SECRET --slots 4
//
// Run it as root, because minibox needs root.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"mbctl/internal/jobs"
)

const maxLogBytes = 64 << 10

// exitTimedOut is the code minibox uses when --timeout kills a job.
const exitTimedOut = 124

func main() {
	server := flag.String("server", "http://127.0.0.1:8787", "coordinator URL")
	token := flag.String("token", os.Getenv("MBD_TOKEN"), "shared secret (or set MBD_TOKEN)")
	name := flag.String("name", hostname(), "name this agent reports as")
	slots := flag.Int("slots", 2, "how many jobs can run at once")
	minibox := flag.String("minibox", "minibox", "path to the minibox binary")
	idle := flag.Duration("interval", 2*time.Second, "how often to poll when there is nothing to do")
	flag.Parse()

	if *token == "" {
		log.Fatal("mbagent: a token is required (--token or MBD_TOKEN)")
	}
	if *slots < 1 {
		log.Fatal("mbagent: --slots must be at least 1")
	}

	c := &client{base: strings.TrimRight(*server, "/"), token: *token, http: &http.Client{Timeout: 20 * time.Second}}
	var running atomic.Int32
	log.Printf("mbagent %s: %d slots, coordinator %s", *name, *slots, c.base)

	for {
		job, err := c.poll(*name, *slots, int(running.Load()))
		if err != nil {
			log.Printf("poll: %v", err)
			time.Sleep(*idle)
			continue
		}
		if job == nil {
			time.Sleep(*idle)
			continue
		}
		running.Add(1)
		log.Printf("running %s: %s", job.ID, strings.Join(job.Spec.Cmd, " "))
		go func(j *jobs.Job) {
			defer running.Add(-1)
			runJob(c, *name, *minibox, j)
		}(job)
	}
}

func runJob(c *client, name, minibox string, j *jobs.Job) {
	cmd := exec.Command(minibox, buildArgs(j.Spec)...)
	out := &tailBuffer{max: maxLogBytes}
	cmd.Stdout = out
	cmd.Stderr = out

	code := 0
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			code = -1
			out.Write([]byte(fmt.Sprintf("mbagent: could not start minibox: %v\n", err)))
		}
	}

	status := jobs.Failed
	switch code {
	case 0:
		status = jobs.Succeeded
	case exitTimedOut:
		status = jobs.TimedOut
	}
	if err := c.report(j.ID, name, status, code, out.String()); err != nil {
		log.Printf("report %s: %v", j.ID, err)
		return
	}
	log.Printf("finished %s: %s (exit %d)", j.ID, status, code)
}

// buildArgs turns a job spec into `minibox run` arguments.
func buildArgs(s jobs.Spec) []string {
	args := []string{"run", "--image", s.Image}
	if s.Net != "" {
		args = append(args, "--net", s.Net)
	}
	if s.MemMB > 0 {
		args = append(args, "--mem", strconv.Itoa(s.MemMB))
	}
	if s.CPUs > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(s.CPUs, 'f', -1, 64))
	}
	if s.Pids > 0 {
		args = append(args, "--pids", strconv.Itoa(s.Pids))
	}
	if s.TimeoutSecs > 0 {
		args = append(args, "--timeout", strconv.Itoa(s.TimeoutSecs))
	}
	if s.Workdir != "" {
		args = append(args, "-w", s.Workdir)
	}
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "-e", k+"="+s.Env[k])
	}
	args = append(args, "--")
	return append(args, s.Cmd...)
}

// tailBuffer keeps the last max bytes written to it, so a chatty job can't fill memory.
type tailBuffer struct {
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.buf) }

// client talks to the coordinator.
type client struct {
	base  string
	token string
	http  *http.Client
}

func (c *client) do(method, path string, body any, out any) (int, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNoContent {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return resp.StatusCode, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(msg)))
	}
	if out != nil && resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

// poll returns the job assigned to this agent, or nil if there is none.
func (c *client) poll(name string, slots, running int) (*jobs.Job, error) {
	var job jobs.Job
	code, err := c.do(http.MethodPost, "/v1/agents/poll",
		map[string]any{"name": name, "slots": slots, "running": running}, &job)
	if err != nil {
		return nil, err
	}
	if code == http.StatusNoContent {
		return nil, nil
	}
	return &job, nil
}

func (c *client) report(id, name string, status jobs.Status, code int, log string) error {
	_, err := c.do(http.MethodPost, "/v1/jobs/"+id+"/report", map[string]any{
		"agent":     name,
		"status":    string(status),
		"exit_code": code,
		"log":       log,
	}, nil)
	return err
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "agent"
	}
	return h
}
