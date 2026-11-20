package coord

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"mbctl/internal/jobs"
)

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
		if _, err := s.Submit(sp); err != nil {
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
