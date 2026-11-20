package coord

import "testing"

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

func TestSubmitRequiresAnOwner(t *testing.T) {
	s := &Store{}
	if _, err := s.Submit(spec(), " "); err == nil {
		t.Fatal("a job without an owner should be rejected")
	}
}
