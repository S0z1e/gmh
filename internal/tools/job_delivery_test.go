package tools

import "testing"

func TestJobRegistryPreservesFinishedResultsUntilDelivery(t *testing.T) {
	var registry JobRegistry
	pendingDelivery := &Job{done: make(chan struct{})}
	registry.add(pendingDelivery)
	pendingDelivery.finish(JobResult{Output: "finished while another session was selected"})

	var oldestDelivered *Job
	for i := 0; i < 50; i++ {
		job := &Job{done: make(chan struct{})}
		registry.add(job)
		job.finish(JobResult{})
		job.MarkDelivered()
		if oldestDelivered == nil {
			oldestDelivered = job
		}
	}
	if registry.Get(pendingDelivery.ID) != pendingDelivery {
		t.Fatalf("evicted %s even though its result was never delivered", pendingDelivery.ID)
	}
	if registry.Get(oldestDelivered.ID) != nil || len(registry.List()) != 50 {
		t.Fatal("expected eviction of the oldest delivered result")
	}
}

func TestJobRegistryTrimsOverflowAfterDelivery(t *testing.T) {
	var registry JobRegistry
	var pending []*Job
	for i := 0; i < 53; i++ {
		job := &Job{done: make(chan struct{})}
		registry.add(job)
		job.finish(JobResult{})
		pending = append(pending, job)
	}
	if count := len(registry.List()); count != 53 {
		t.Fatalf("retained %d results, want all 53 pending deliveries", count)
	}
	for _, job := range pending[:3] {
		job.MarkDelivered()
	}
	kept := registry.List()
	if len(kept) != 50 || kept[0] != pending[3] {
		t.Fatalf("retained %d results after delivery; want the last 50", len(kept))
	}
}
