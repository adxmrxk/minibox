package coord

import (
	"strings"
	"testing"

	"mbctl/internal/jobs"
)

func TestListOnlyReturnsTheOwnersJobs(t *testing.T) {
	s := newStore(t)
	mine, _ := s.Submit(spec(), "alice")
	s.Submit(spec(), "bob")

	list, err := s.List("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != mine.ID || list[0].Owner != "alice" {
		t.Fatalf("alice should see only her job, got %+v", list)
	}
}

// A report with a log bigger than etcd accepts (about 1.5 MB per value) must still be recorded.
func TestOversizedLogIsCappedAndRecorded(t *testing.T) {
	s := newStore(t)
	j, _ := s.Submit(spec(), "tester")
	if _, err := s.Poll("agent-a", 1, 0); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("x", 2<<20)
	if err := s.Report(j.ID, "agent-a", jobs.Succeeded, 0, big); err != nil {
		t.Fatalf("report with a 2 MB log was refused: %v", err)
	}
	got, _ := s.Get(j.ID)
	if got.Status != jobs.Succeeded {
		t.Fatalf("job not finished: %s", got.Status)
	}
	if len(got.Log) > MaxLogBytes {
		t.Fatalf("log stored at %d bytes, limit is %d", len(got.Log), MaxLogBytes)
	}
}

// A user's own jobs must stay visible even when other users have submitted many newer ones.
func TestListFindsAnOldJobEvenAfterManyNewerJobsByOthers(t *testing.T) {
	s := newStore(t)
	mine, err := s.Submit(spec(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < ListLimit+50; i++ {
		if _, err := s.Submit(spec(), "bob"); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.List("alice")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, j := range list {
		if j.ID == mine.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("alice's job %s is missing from her list (got %d jobs)", mine.ID, len(list))
	}
}

func TestSubmitRequiresAnOwner(t *testing.T) {
	s := &Store{}
	if _, err := s.Submit(spec(), " "); err == nil {
		t.Fatal("a job without an owner should be rejected")
	}
}
