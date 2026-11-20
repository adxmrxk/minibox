package coord

import (
	"testing"
	"time"
)

// A job claimed by a live agent must stay assigned after the requeue interval passes. The agent
// keeps its lease by polling, as a real agent does.
func TestLiveAgentJobIsNotRequeued(t *testing.T) {
	s := newStore(t)
	s.LeaseTTL = 30 * time.Second
	j, err := s.Submit(spec(), "tester")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.PollWait("agent-live", 1, 0, 5*time.Second)
	if err != nil || got == nil {
		t.Fatalf("claim failed: %v %v", got, err)
	}
	time.Sleep(requeueEvery + 500*time.Millisecond)
	if _, err := s.Poll("agent-live", 1, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Poll("agent-other", 1, 0); err != nil {
		t.Fatal(err)
	}
	cur, ok := s.Get(j.ID)
	if !ok {
		t.Fatal("job missing")
	}
	if cur.Status != "assigned" {
		t.Fatalf("job status is %q after requeue interval, want assigned (agent=%q)", cur.Status, cur.Agent)
	}
}
