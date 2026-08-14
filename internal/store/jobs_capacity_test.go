package store

import "testing"

func TestCountActiveJobsIsByCapacityNotItem(t *testing.T) {
	s := newTestStore(t)
	ws := &Workspace{DisplayName: "w", CapacityID: DefaultCapacityID}
	if err := s.CreateWorkspace(ws, Principal{ID: "p", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	it := &Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lh"}
	if err := s.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}

	running := &JobInstance{ItemID: it.ID, JobType: "DefaultJob", CompleteAt: s.Now() + 60}
	if err := s.CreateJobInstance(running); err != nil {
		t.Fatal(err)
	}
	queued := &JobInstance{
		ItemID: it.ID, JobType: "DefaultJob", InvokeType: InvokeScheduled,
		Queued: true, CompleteAt: 1 << 62,
	}
	if err := s.CreateJobInstance(queued); err != nil {
		t.Fatal(err)
	}

	n, err := s.CountActiveJobsOnCapacity(DefaultCapacityID)
	if err != nil || n != 1 {
		t.Fatalf("active = %d, %v; queued jobs must not occupy a slot", n, err)
	}
	waiting, err := s.ListQueuedJobs()
	if err != nil || len(waiting) != 1 || waiting[0].ID != queued.ID {
		t.Fatalf("queued = %+v, %v", waiting, err)
	}

	// Two admitted runs of the same item both occupy slots (docs/36).
	second := &JobInstance{ItemID: it.ID, JobType: "DefaultJob", CompleteAt: s.Now() + 60}
	if err := s.CreateJobInstance(second); err != nil {
		t.Fatal(err)
	}
	n, err = s.CountActiveJobsOnCapacity(DefaultCapacityID)
	if err != nil || n != 2 {
		t.Fatalf("two same-item runs = %d, %v; want 2", n, err)
	}

	if err := s.AdmitQueuedJob(queued.ID, s.Now()+60); err != nil {
		t.Fatal(err)
	}
	n, err = s.CountActiveJobsOnCapacity(DefaultCapacityID)
	if err != nil || n != 3 {
		t.Fatalf("after admit = %d, %v; want 3", n, err)
	}
	waiting, err = s.ListQueuedJobs()
	if err != nil || len(waiting) != 0 {
		t.Fatalf("queued after admit = %+v, %v", waiting, err)
	}
}
