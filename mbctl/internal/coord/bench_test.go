package coord

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"mbctl/internal/jobs"
)

// BenchmarkPollWithAssigned measures what an idle agent's poll costs while the cluster holds
// many assigned jobs. Every poll has to check that each assigned job's agent is still alive.
func BenchmarkPollWithAssigned(b *testing.B) {
	raw := os.Getenv("ETCD_ENDPOINTS")
	if raw == "" {
		b.Skip("set ETCD_ENDPOINTS to run the benchmark")
	}
	const assigned = 500
	s, err := Open(strings.Split(raw, ","), fmt.Sprintf("/bench-poll-%d", time.Now().UnixNano()))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	sp := jobs.Spec{Image: "ci", Cmd: []string{"true"}}
	for i := 0; i < assigned; i++ {
		if _, err := s.Submit(sp, "bench"); err != nil {
			b.Fatal(err)
		}
		// One agent per job, so every assigned job has its own agent to check.
		if _, err := s.Poll(fmt.Sprintf("holder-%d", i), 1, 0); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Poll("idle-agent", 1, 1); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSubmitAndClaim measures the path a busy coordinator runs most: queue a job,
// claim it, and report it. Needs ETCD_ENDPOINTS, like the other store tests.
func BenchmarkSubmitAndClaim(b *testing.B) {
	raw := os.Getenv("ETCD_ENDPOINTS")
	if raw == "" {
		b.Skip("set ETCD_ENDPOINTS to run the benchmark")
	}
	eps := strings.Split(raw, ",")
	s, err := Open(eps, fmt.Sprintf("/bench-%d", time.Now().UnixNano()))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	sp := jobs.Spec{Image: "ci", Cmd: []string{"true"}}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Submit(sp, "bench"); err != nil {
			b.Fatal(err)
		}
		job, err := s.Poll("bench-agent", 1, 0)
		if err != nil || job == nil {
			b.Fatalf("claim failed: %v", err)
		}
		if err := s.Report(job.ID, "bench-agent", jobs.Succeeded, 0, ""); err != nil {
			b.Fatal(err)
		}
	}
}
