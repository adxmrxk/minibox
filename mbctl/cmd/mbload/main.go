// mbload drives load through the coordinator to measure its control plane.
//
// It submits jobs over HTTPS and runs simulated agents that poll and report. A simulated agent
// finishes each job instantly, so the numbers measure the coordinator and etcd, not sandbox
// start-up. Latency is submit to finish, so it includes time spent waiting in the queue.
//
//	mbload --server https://127.0.0.1:8800 --ca mbd.crt --run R1 --agents 50 --jobs 3000 --submitters 16
package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type job struct {
	ID string `json:"id"`
}

func main() {
	server := flag.String("server", "https://127.0.0.1:8800", "coordinator URL")
	caFile := flag.String("ca", "", "CA file for the coordinator certificate")
	run := flag.String("run", "run", "run id; agent and user tokens are derived from it")
	agents := flag.Int("agents", 10, "simulated agents")
	slots := flag.Int("slots", 2, "slots per simulated agent")
	jobs := flag.Int("jobs", 1000, "jobs to submit")
	submitters := flag.Int("submitters", 8, "concurrent submitters")
	idle := flag.Duration("idle", 500*time.Millisecond, "how long an agent waits after an empty poll")
	timeout := flag.Duration("timeout", 10*time.Minute, "give up after this long")
	flag.Parse()

	client := newClient(*caFile)
	userToken := "load-user-" + *run
	base := strings.TrimRight(*server, "/")

	var (
		mu        sync.Mutex
		submitted = map[string]time.Time{}
		// early holds finish times for jobs reported before their submit recorded its start.
		// Without this, such a job would count as finished but get no latency.
		early     = map[string]time.Time{}
		latencies []time.Duration
		finished  atomic.Int64
		errs      atomic.Int64
		firstSub  time.Time
		lastFin   time.Time
	)
	done := make(chan struct{})

	// Submitters: send jobs as fast as allowed, spread over several goroutines.
	var sw sync.WaitGroup
	perSubmitter := *jobs / *submitters
	for s := 0; s < *submitters; s++ {
		sw.Add(1)
		go func(n int) {
			defer sw.Done()
			for i := 0; i < n; i++ {
				start := time.Now()
				id, err := submit(client, base, userToken)
				if err != nil {
					errs.Add(1)
					continue
				}
				mu.Lock()
				submitted[id] = start
				if fin, ok := early[id]; ok {
					latencies = append(latencies, fin.Sub(start))
					lastFin = fin
					delete(early, id)
				}
				if firstSub.IsZero() || start.Before(firstSub) {
					firstSub = start
				}
				mu.Unlock()
			}
		}(perSubmitter)
	}

	// Simulated agents: poll, then report each job as succeeded straight away.
	var aw sync.WaitGroup
	for a := 1; a <= *agents; a++ {
		aw.Add(1)
		name := fmt.Sprintf("load-%s-%03d", *run, a)
		token := fmt.Sprintf("agent-%s-%03d", *run, a)
		go func() {
			defer aw.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				id, err := poll(client, base, token, name, *slots)
				if err != nil {
					errs.Add(1)
					time.Sleep(*idle)
					continue
				}
				if id == "" {
					// Held open by the coordinator, so poll again straight away.
					continue
				}
				if err := report(client, base, token, name, id); err != nil {
					errs.Add(1)
					continue
				}
				now := time.Now()
				mu.Lock()
				if t, ok := submitted[id]; ok {
					latencies = append(latencies, now.Sub(t))
					lastFin = now
				} else {
					early[id] = now
				}
				mu.Unlock()
				finished.Add(1)
			}
		}()
	}

	// Wait for the submitters, then for every job that was accepted to finish. Jobs whose
	// submit failed are not waited for, so a failed submit cannot stall the run.
	deadline := time.After(*timeout)
	sw.Wait()
	mu.Lock()
	accepted := int64(len(submitted))
	mu.Unlock()
	for finished.Load() < accepted {
		select {
		case <-deadline:
			log.Printf("timed out with %d of %d accepted jobs finished", finished.Load(), accepted)
			goto stop
		case <-time.After(20 * time.Millisecond):
		}
	}
stop:
	close(done)
	aw.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(latencies) == 0 {
		log.Fatal("no jobs finished")
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	elapsed := lastFin.Sub(firstSub)
	fmt.Printf("agents=%d submitters=%d jobs_submitted=%d jobs_finished=%d errors=%d\n",
		*agents, *submitters, len(submitted), len(latencies), errs.Load())
	fmt.Printf("elapsed_s=%.2f throughput_jobs_per_s=%.1f\n", elapsed.Seconds(), float64(len(latencies))/elapsed.Seconds())
	fmt.Printf("latency_ms p50=%.1f p95=%.1f p99=%.1f max=%.1f\n",
		ms(pct(latencies, 0.50)), ms(pct(latencies, 0.95)), ms(pct(latencies, 0.99)), ms(latencies[len(latencies)-1]))
	_ = userToken
}

func pct(sorted []time.Duration, p float64) time.Duration {
	i := int(float64(len(sorted)-1) * p)
	return sorted[i]
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func newClient(caFile string) *http.Client {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			log.Fatal(err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			log.Fatal("bad CA file")
		}
		tlsCfg.RootCAs = pool
	}
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsCfg, MaxIdleConnsPerHost: 256},
	}
}

func do(c *http.Client, method, url, token string, body any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out, nil
}

func submit(c *http.Client, base, token string) (string, error) {
	code, body, err := do(c, http.MethodPost, base+"/v1/jobs", token,
		map[string]any{"image": "ci", "cmd": []string{"true"}, "net": "isolated"})
	if err != nil {
		return "", err
	}
	if code != http.StatusCreated {
		return "", fmt.Errorf("submit: HTTP %d: %s", code, body)
	}
	var j job
	if err := json.Unmarshal(body, &j); err != nil {
		return "", err
	}
	return j.ID, nil
}

func poll(c *http.Client, base, token, name string, slots int) (string, error) {
	code, body, err := do(c, http.MethodPost, base+"/v1/agents/poll", token,
		map[string]any{"name": name, "slots": slots, "running": 0})
	if err != nil {
		return "", err
	}
	if code == http.StatusNoContent {
		return "", nil
	}
	if code != http.StatusOK {
		return "", fmt.Errorf("poll: HTTP %d: %s", code, body)
	}
	var j job
	if err := json.Unmarshal(body, &j); err != nil {
		return "", err
	}
	return j.ID, nil
}

func report(c *http.Client, base, token, name, id string) error {
	code, body, err := do(c, http.MethodPost, base+"/v1/jobs/"+id+"/report", token,
		map[string]any{"agent": name, "status": "succeeded", "exit_code": 0, "log": ""})
	if err != nil {
		return err
	}
	if code != http.StatusNoContent {
		return fmt.Errorf("report: HTTP %d: %s", code, body)
	}
	return nil
}
