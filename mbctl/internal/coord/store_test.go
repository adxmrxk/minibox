package coord

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mbctl/internal/jobs"
)

// The tests need a real etcd. Set ETCD_ENDPOINTS (for example http://127.0.0.1:2379) to run them.
func endpoints(t *testing.T) []string {
	t.Helper()
	raw := os.Getenv("ETCD_ENDPOINTS")
	if raw == "" {
		t.Skip("set ETCD_ENDPOINTS to run the etcd tests")
	}
	return strings.Split(raw, ",")
}

var testCounter atomic.Int64

// newStore opens a store under its own key prefix, so tests never see each other's jobs.
func newStore(t *testing.T) *Store {
	t.Helper()
	eps := endpoints(t)
	prefix := fmt.Sprintf("/test-%d-%d", time.Now().UnixNano(), testCounter.Add(1))
	s, err := Open(eps, prefix)
	if err != nil {
		t.Fatal(err)
	}
	s.LeaseTTL = 3 * time.Second
	t.Cleanup(func() { s.Close() })
	return s
}

func spec() jobs.Spec {
	return jobs.Spec{Image: "ci", Cmd: []string{"echo", "hi"}}
}

func mustMetrics(t *testing.T, s *Store) string {
	t.Helper()
	m, err := s.Metrics()
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestSubmitRejectsJobsWithoutImage(t *testing.T) {
	// Validation runs before anything reaches etcd, so this needs no cluster.
	s := &Store{}
	if _, err := s.Submit(jobs.Spec{Cmd: []string{"true"}}, "tester"); err == nil {
		t.Fatal("expected an error for a job with no image")
	}
}

func TestPollAssignsOldestQueuedJobWithinSlots(t *testing.T) {
	s := newStore(t)
	first, err := s.Submit(spec(), "tester")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(spec(), "tester"); err != nil {
		t.Fatal(err)
	}

	job, err := s.Poll("agent-a", 1, 0)
	if err != nil || job == nil {
		t.Fatalf("expected a job, got %v, %v", job, err)
	}
	if job.ID != first.ID || job.Status != jobs.Assigned || job.Agent != "agent-a" {
		t.Fatalf("wrong assignment: %+v", job)
	}

	// No free slot left, so nothing more is handed out.
	if job, _ := s.Poll("agent-a", 1, 1); job != nil {
		t.Fatalf("agent with no free slot got %s", job.ID)
	}
}

func TestReportOnlyAcceptsTheAssignedAgent(t *testing.T) {
	s := newStore(t)
	j, _ := s.Submit(spec(), "tester")
	s.Poll("agent-a", 1, 0)

	if err := s.Report(j.ID, "agent-b", jobs.Succeeded, 0, ""); err == nil {
		t.Fatal("a different agent should not be able to report the job")
	}
	if err := s.Report(j.ID, "agent-a", jobs.Succeeded, 0, "ok"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(j.ID)
	if got.Status != jobs.Succeeded || got.Log != "ok" || got.ExitCode == nil || *got.ExitCode != 0 {
		t.Fatalf("result not recorded: %+v", got)
	}
	if err := s.Report(j.ID, "agent-a", jobs.Failed, 1, ""); err == nil {
		t.Fatal("a finished job should not accept a second report")
	}
}

// An agent whose lease expires is treated as dead, and its job goes to another agent.
func TestDeadAgentJobsAreRequeued(t *testing.T) {
	s := newStore(t)
	j, _ := s.Submit(spec(), "tester")
	s.Poll("agent-a", 1, 0)

	// agent-a stops polling. Its lease runs out after LeaseTTL.
	time.Sleep(s.LeaseTTL + time.Second)

	job, err := s.Poll("agent-b", 1, 0)
	if err != nil || job == nil {
		t.Fatalf("expected the requeued job, got %v, %v", job, err)
	}
	if job.ID != j.ID || job.Agent != "agent-b" {
		t.Fatalf("job was not reassigned to agent-b: %+v", job)
	}
}

// A live agent keeps its jobs, because its lease keeps getting renewed.
func TestLiveAgentKeepsItsJob(t *testing.T) {
	s := newStore(t)
	j, _ := s.Submit(spec(), "tester")
	s.Poll("agent-a", 1, 0)
	for i := 0; i < 3; i++ {
		time.Sleep(s.LeaseTTL / 2)
		s.Poll("agent-a", 1, 1)
	}
	got, _ := s.Get(j.ID)
	if got.Status != jobs.Assigned || got.Agent != "agent-a" {
		t.Fatalf("live agent lost its job: %+v", got)
	}
	if job, _ := s.Poll("agent-b", 1, 0); job != nil {
		t.Fatalf("job was handed to a second agent while the first was alive: %s", job.ID)
	}
}

func TestStateSurvivesRestart(t *testing.T) {
	s := newStore(t)
	j, _ := s.Submit(spec(), "tester")
	prefix := s.prefix
	s.Close()

	reopened, err := Open(endpoints(t), prefix)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, ok := reopened.Get(j.ID)
	if !ok || got.Status != jobs.Queued {
		t.Fatalf("job lost across restart: %+v, %v", got, ok)
	}
	next, _ := reopened.Submit(spec(), "tester")
	if next.ID == j.ID {
		t.Fatal("job IDs repeated after restart")
	}
}

// Many agents polling at once must never be handed the same job.
func TestConcurrentPollsNeverDoubleAssign(t *testing.T) {
	s := newStore(t)
	const total = 40
	for i := 0; i < total; i++ {
		if _, err := s.Submit(spec(), "tester"); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	seen := map[string]string{}
	var wg sync.WaitGroup
	for a := 0; a < 8; a++ {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			for {
				job, err := s.Poll(name, 1, 0)
				if err != nil {
					t.Errorf("poll: %v", err)
					return
				}
				if job == nil {
					return
				}
				mu.Lock()
				if prev, dup := seen[job.ID]; dup {
					t.Errorf("%s handed to %s and %s", job.ID, prev, name)
				}
				seen[job.ID] = name
				mu.Unlock()
				if err := s.Report(job.ID, name, jobs.Succeeded, 0, ""); err != nil {
					t.Errorf("report: %v", err)
					return
				}
			}
		}(fmt.Sprintf("agent-%d", a))
	}
	wg.Wait()
	if len(seen) != total {
		t.Fatalf("expected %d jobs assigned exactly once, got %d", total, len(seen))
	}
}

func TestListReturnsNewestFirst(t *testing.T) {
	s := newStore(t)
	a, _ := s.Submit(spec(), "tester")
	b, _ := s.Submit(spec(), "tester")
	list, err := s.List("tester")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != b.ID || list[1].ID != a.ID {
		t.Fatalf("unexpected order: %+v", list)
	}
}

func TestMetricsCountJobsAndAgents(t *testing.T) {
	s := newStore(t)
	s.Submit(spec(), "tester")
	s.Submit(spec(), "tester")
	s.Poll("agent-a", 4, 1)

	m := mustMetrics(t, s)
	for _, want := range []string{
		`minibox_jobs{status="queued"} 1`,
		`minibox_jobs{status="assigned"} 1`,
		"minibox_agents_online 1",
		"minibox_agent_slots 4",
		"minibox_agent_slots_busy 1",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics missing %q\n%s", want, m)
		}
	}
}
