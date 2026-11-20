package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"mbctl/internal/jobs"
)

// TestMain lets the test binary stand in for minibox: when the env flag is set it just exits 0.
func TestMain(m *testing.M) {
	if os.Getenv("MBAGENT_FAKE_MINIBOX") == "1" {
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// TestReportsSurviveTransientCoordinatorErrors runs 100 jobs against a coordinator that
// rejects the first report attempt for every third job with 503, the way a coordinator
// behind a restart or a brief quorum loss would. It counts how many finished jobs the
// coordinator never recorded.
func TestReportsSurviveTransientCoordinatorErrors(t *testing.T) {
	t.Setenv("MBAGENT_FAKE_MINIBOX", "1")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	const total = 100
	var mu sync.Mutex
	attempts := map[string]int{}
	recorded := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/report") {
			http.NotFound(w, r)
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/jobs/"), "/report")
		mu.Lock()
		defer mu.Unlock()
		attempts[id]++
		var n int
		fmt.Sscanf(id, "job-%d", &n)
		if n%3 == 0 && attempts[id] == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		recorded[id] = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := &client{base: srv.URL, token: "t", http: srv.Client()}
	for i := 1; i <= total; i++ {
		job := &jobs.Job{ID: fmt.Sprintf("job-%d", i), Spec: jobs.Spec{Image: "ci", Cmd: []string{"true"}}}
		runJob(c, "test-host", self, job)
	}

	lost := 0
	for i := 1; i <= total; i++ {
		if !recorded[fmt.Sprintf("job-%d", i)] {
			lost++
		}
	}
	t.Logf("jobs finished: %d, results the coordinator never recorded: %d", total, lost)
	if lost > 0 {
		t.Fatalf("%d of %d finished jobs were never reported", lost, total)
	}
}
