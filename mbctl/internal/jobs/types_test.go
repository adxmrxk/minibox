package jobs

import "testing"

func TestSpecValidation(t *testing.T) {
	good := Spec{Image: "ci", Cmd: []string{"true"}, Net: "bridge", Workdir: "/work"}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	bad := []Spec{
		{Cmd: []string{"true"}}, // no image
		{Image: "ci"},           // no command
		{Image: "ci", Cmd: []string{"true"}, Net: "vlan"},         // unknown network mode
		{Image: "ci", Cmd: []string{"true"}, MemMB: -1},           // negative limit
		{Image: "ci", Cmd: []string{"true"}, Workdir: "relative"}, // relative workdir
	}
	for i, s := range bad {
		if s.Validate() == nil {
			t.Errorf("case %d should have been rejected: %+v", i, s)
		}
	}
}

func TestFinishedStatuses(t *testing.T) {
	if Queued.Finished() || Assigned.Finished() {
		t.Fatal("queued and assigned jobs are not finished")
	}
	if !Succeeded.Finished() || !Failed.Finished() || !TimedOut.Finished() {
		t.Fatal("final statuses must report finished")
	}
}
