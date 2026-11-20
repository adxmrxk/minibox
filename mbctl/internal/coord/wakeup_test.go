package coord

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// A poll that starts before any job exists must return as soon as one is queued.
func TestPollWaitWakesWhenJobIsQueued(t *testing.T) {
	s := newStore(t)
	go func() {
		time.Sleep(300 * time.Millisecond)
		s.Submit(spec(), "tester")
	}()
	start := time.Now()
	job, err := s.PollWait("agent-a", 1, 0, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if job == nil {
		t.Fatalf("poll returned no job after %v; a queued job should wake the waiter", time.Since(start))
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("poll took %v; expected a prompt wakeup", time.Since(start))
	}
}

// Plain Poll, no waiting: concurrent claims must still give each job to one agent.
func TestConcurrentPollClaimsEachJobOnce(t *testing.T) {
	s := newStore(t)
	const agents, jobsN = 5, 3
	for i := 0; i < jobsN; i++ {
		if _, err := s.Submit(spec(), "tester"); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	got := map[string]int{}
	var wg sync.WaitGroup
	for i := 0; i < agents; i++ {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			j, err := s.Poll(name, 1, 0)
			if err != nil {
				t.Errorf("%s: %v", name, err)
				return
			}
			if j != nil {
				mu.Lock()
				got[j.ID]++
				mu.Unlock()
			}
		}(fmt.Sprintf("agent-%d", i))
	}
	wg.Wait()
	for id, n := range got {
		if n != 1 {
			t.Fatalf("job %s claimed %d times (plain Poll)", id, n)
		}
	}
	if len(got) != jobsN {
		t.Fatalf("%d jobs claimed, want %d", len(got), jobsN)
	}
}

// Several waiting agents and several jobs: each job goes to exactly one agent.
func TestPollWaitHandsEachJobToOneWaiter(t *testing.T) {
	s := newStore(t)
	// Claimed agents stop polling once they have a job, so keep their leases from expiring
	// during the test. Expiry is tested separately.
	s.LeaseTTL = 30 * time.Second
	const agents, jobsN = 5, 3
	var mu sync.Mutex
	got := map[string]int{}
	var wg sync.WaitGroup
	for i := 0; i < agents; i++ {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			j, err := s.PollWait(name, 1, 0, 5*time.Second)
			if err != nil {
				t.Errorf("%s: %v", name, err)
				return
			}
			if j != nil {
				mu.Lock()
				got[j.ID]++
				t.Logf("%s returned %s (job.Agent=%s, status=%s)", name, j.ID, j.Agent, j.Status)
				mu.Unlock()
			}
		}(fmt.Sprintf("agent-%d", i))
	}
	time.Sleep(300 * time.Millisecond)
	for i := 0; i < jobsN; i++ {
		if _, err := s.Submit(spec(), "tester"); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	if len(got) != jobsN {
		t.Fatalf("%d distinct jobs claimed, want %d: %v", len(got), jobsN, got)
	}
	for id, n := range got {
		if n != 1 {
			t.Fatalf("job %s claimed %d times", id, n)
		}
	}
}
