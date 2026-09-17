package investigation

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

func TestReachableTargetsIgnoresCyclesSelfLoopsAndParallelEdges(t *testing.T) {
	edges := []Edge{
		{From: 0, To: 2},
		{From: 0, To: 2}, // parallel edge
		{From: 2, To: 3},
		{From: 3, To: 0}, // cycle back to an inlet
		{From: 1, To: 3},
		{From: 3, To: 3}, // self-loop
		{From: 0, To: 0}, // self-loop on an inlet
		{From: 5, To: 4}, // unreachable from the inlets
	}
	inv := NewStore().Create(6, edges, []int64{0, 1}, fixedNow)

	want := []int64{2, 3}
	if fmt.Sprint(inv.Targets) != fmt.Sprint(want) {
		t.Fatalf("targets = %v, want %v", inv.Targets, want)
	}
	if inv.Status != StatusPending {
		t.Fatalf("status = %s, want PENDING", inv.Status)
	}
	if inv.Conclusion != ConclusionClean {
		t.Fatalf("conclusion = %s, want clean", inv.Conclusion)
	}
	if len(inv.Samples) != 0 {
		t.Fatalf("samples = %v, want none", inv.Samples)
	}
}

func TestReachableTargetsExcludesInletsReachedThroughOthers(t *testing.T) {
	// Inlet 1 is reachable from inlet 0 but must not become a target.
	inv := NewStore().Create(3, []Edge{{From: 0, To: 1}, {From: 1, To: 2}, {From: 2, To: 0}}, []int64{0, 1}, fixedNow)
	want := []int64{2}
	if fmt.Sprint(inv.Targets) != fmt.Sprint(want) {
		t.Fatalf("targets = %v, want %v", inv.Targets, want)
	}
}

func TestCreateWithoutTargetsCompletesImmediately(t *testing.T) {
	cases := []struct {
		name   string
		n      int64
		edges  []Edge
		inlets []int64
	}{
		{"no edges at all", 2, nil, []int64{0}},
		{"only self loops", 2, []Edge{{From: 0, To: 0}}, []int64{0}},
		{"edges only between inlets", 3, []Edge{{From: 0, To: 1}, {From: 1, To: 0}}, []int64{0, 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewStore()
			inv := store.Create(tc.n, tc.edges, tc.inlets, fixedNow)
			if inv.Status != StatusCompleted {
				t.Fatalf("status = %s, want COMPLETED", inv.Status)
			}
			if len(inv.Targets) != 0 {
				t.Fatalf("targets = %v, want none", inv.Targets)
			}
			if inv.Conclusion != ConclusionClean {
				t.Fatalf("conclusion = %s, want clean", inv.Conclusion)
			}
			// Terminal from birth: any write is rejected.
			_, err := store.AddSample(inv.ID, Sample{SampleID: "s1", Node: 0, Result: ResultClean})
			if !errors.Is(err, ErrCompleted) {
				t.Fatalf("AddSample on completed investigation = %v, want ErrCompleted", err)
			}
		})
	}
}

func TestCreateSnapshotsAndNormalizesInput(t *testing.T) {
	store := NewStore()
	edges := []Edge{{From: 0, To: 1}}
	inlets := []int64{2, 0, 0}
	inv := store.Create(3, edges, inlets, fixedNow)

	// Inlets are stored sorted and deduplicated.
	if fmt.Sprint(inv.Inlets) != "[0 2]" {
		t.Fatalf("inlets = %v, want [0 2]", inv.Inlets)
	}
	// Mutating the caller's slices must not corrupt the snapshot.
	edges[0].To = 2
	inlets[0] = 1
	if inv.Edges[0].To != 1 || inv.Inlets[0] != 0 {
		t.Fatalf("snapshot aliases caller memory: %+v", inv)
	}
	// The returned copy must not alias the stored investigation either.
	inv.Targets[0] = 99
	again, err := store.AddSample(inv.ID, Sample{SampleID: "s", Node: 1, Result: ResultClean})
	if err != nil {
		t.Fatalf("AddSample: %v", err)
	}
	if again.Targets[0] == 99 {
		t.Fatal("returned view aliases stored state")
	}
}

func TestAddSampleLifecycleAndConclusion(t *testing.T) {
	store := NewStore()
	inv := store.Create(4, []Edge{{From: 0, To: 1}, {From: 1, To: 2}, {From: 0, To: 3}}, []int64{0}, fixedNow)
	if inv.Status != StatusPending {
		t.Fatalf("status = %s, want PENDING", inv.Status)
	}

	inv, err := store.AddSample(inv.ID, Sample{SampleID: "a", Node: 1, Result: ResultClean})
	if err != nil {
		t.Fatalf("AddSample a: %v", err)
	}
	if inv.Status != StatusInProgress {
		t.Fatalf("status = %s, want IN_PROGRESS", inv.Status)
	}

	if _, err := store.AddSample(inv.ID, Sample{SampleID: "b", Node: 2, Result: ResultContaminated}); err != nil {
		t.Fatalf("AddSample b: %v", err)
	}

	inv, err = store.AddSample(inv.ID, Sample{SampleID: "c", Node: 3, Result: ResultClean})
	if err != nil {
		t.Fatalf("AddSample c: %v", err)
	}
	if inv.Status != StatusCompleted {
		t.Fatalf("status = %s, want COMPLETED", inv.Status)
	}
	if inv.Conclusion != ConclusionContaminated {
		t.Fatalf("conclusion = %s, want contaminated", inv.Conclusion)
	}
	if len(inv.Samples) != 3 {
		t.Fatalf("samples = %d, want 3", len(inv.Samples))
	}

	// Terminal state never regresses and rejects further writes.
	if _, err := store.AddSample(inv.ID, Sample{SampleID: "d", Node: 1, Result: ResultClean}); !errors.Is(err, ErrCompleted) {
		t.Fatalf("write after completion = %v, want ErrCompleted", err)
	}
}

func TestAddSampleAllCleanConcludesClean(t *testing.T) {
	store := NewStore()
	inv := store.Create(2, []Edge{{From: 0, To: 1}}, []int64{0}, fixedNow)
	inv, err := store.AddSample(inv.ID, Sample{SampleID: "only", Node: 1, Result: ResultClean})
	if err != nil {
		t.Fatalf("AddSample: %v", err)
	}
	if inv.Status != StatusCompleted || inv.Conclusion != ConclusionClean {
		t.Fatalf("got (%s, %s), want (COMPLETED, clean)", inv.Status, inv.Conclusion)
	}
}

func TestAddSampleConflicts(t *testing.T) {
	store := NewStore()
	inv := store.Create(5, []Edge{{From: 0, To: 1}, {From: 0, To: 2}}, []int64{0}, fixedNow)
	if _, err := store.AddSample(inv.ID, Sample{SampleID: "first", Node: 1, Result: ResultClean}); err != nil {
		t.Fatalf("AddSample first: %v", err)
	}

	cases := []struct {
		name   string
		id     string
		sample Sample
		want   error
	}{
		{"unknown investigation", "no-such-id", Sample{SampleID: "x", Node: 2, Result: ResultClean}, ErrNotFound},
		{"duplicate sample_id", inv.ID, Sample{SampleID: "first", Node: 2, Result: ResultClean}, ErrDuplicateSampleID},
		{"node already sampled", inv.ID, Sample{SampleID: "second", Node: 1, Result: ResultClean}, ErrNodeAlreadySampled},
		{"node not a target", inv.ID, Sample{SampleID: "third", Node: 3, Result: ResultClean}, ErrNodeNotTarget},
		{"node out of range", inv.ID, Sample{SampleID: "fourth", Node: 4, Result: ResultClean}, ErrNodeNotTarget},
		{"inlet is not a target", inv.ID, Sample{SampleID: "fifth", Node: 0, Result: ResultClean}, ErrNodeNotTarget},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.AddSample(tc.id, tc.sample); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}

	// Rejected writes leave no trace: the investigation still has exactly one
	// sample and accepts the remaining target.
	inv, err := store.AddSample(inv.ID, Sample{SampleID: "last", Node: 2, Result: ResultClean})
	if err != nil {
		t.Fatalf("AddSample last: %v", err)
	}
	if len(inv.Samples) != 2 || inv.Status != StatusCompleted {
		t.Fatalf("samples = %d status = %s, want 2 samples and COMPLETED", len(inv.Samples), inv.Status)
	}
}

func TestSampleIDsAreGloballyUnique(t *testing.T) {
	store := NewStore()
	a := store.Create(2, []Edge{{From: 0, To: 1}}, []int64{0}, fixedNow)
	b := store.Create(2, []Edge{{From: 0, To: 1}}, []int64{0}, fixedNow)
	if a.ID == b.ID {
		t.Fatal("investigation ids must differ")
	}
	if _, err := store.AddSample(a.ID, Sample{SampleID: "shared", Node: 1, Result: ResultClean}); err != nil {
		t.Fatalf("AddSample on a: %v", err)
	}
	if _, err := store.AddSample(b.ID, Sample{SampleID: "shared", Node: 1, Result: ResultClean}); !errors.Is(err, ErrDuplicateSampleID) {
		t.Fatalf("reused sample_id across investigations = %v, want ErrDuplicateSampleID", err)
	}
}

// TestConcurrentDistinctTargets hammers one investigation with one sample per
// target plus exact duplicates racing them: every target must be recorded
// exactly once and the investigation must conclude exactly once.
func TestConcurrentDistinctTargets(t *testing.T) {
	const targets = 40
	edges := make([]Edge, 0, targets)
	for i := 0; i < targets; i++ {
		edges = append(edges, Edge{From: int64(i), To: int64(i + 1)})
	}
	store := NewStore()
	inv := store.Create(targets+1, edges, []int64{0}, fixedNow)
	if len(inv.Targets) != targets {
		t.Fatalf("targets = %d, want %d", len(inv.Targets), targets)
	}

	type outcome struct {
		inv *Investigation
		err error
	}
	results := make(chan outcome, 2*targets)
	var wg sync.WaitGroup
	fire := func(sample Sample) {
		defer wg.Done()
		got, err := store.AddSample(inv.ID, sample)
		results <- outcome{got, err}
	}
	for node := int64(1); node <= targets; node++ {
		result := ResultClean
		if node == 20 {
			result = ResultContaminated
		}
		sample := Sample{SampleID: fmt.Sprintf("s-%d", node), Node: node, Result: result}
		wg.Add(1)
		go fire(sample)
		// An exact duplicate racing the original must be rejected.
		wg.Add(1)
		go fire(sample)
	}
	wg.Wait()
	close(results)

	var succeeded, completedViews int
	var final *Investigation
	for out := range results {
		if out.err != nil {
			// A duplicate loses to the original sample, or arrives after
			// the investigation completed; both are valid rejections.
			if !errors.Is(out.err, ErrDuplicateSampleID) &&
				!errors.Is(out.err, ErrNodeAlreadySampled) &&
				!errors.Is(out.err, ErrCompleted) {
				t.Fatalf("unexpected error: %v", out.err)
			}
			continue
		}
		succeeded++
		if out.inv.Status == StatusCompleted {
			completedViews++
			final = out.inv
		}
	}
	if succeeded != targets {
		t.Fatalf("succeeded = %d, want %d", succeeded, targets)
	}
	if completedViews != 1 {
		t.Fatalf("completed views = %d, want exactly 1", completedViews)
	}
	if len(final.Samples) != targets {
		t.Fatalf("final samples = %d, want %d", len(final.Samples), targets)
	}
	seenIDs := make(map[string]bool)
	seenNodes := make(map[int64]bool)
	for _, s := range final.Samples {
		if seenIDs[s.SampleID] {
			t.Fatalf("duplicate sample recorded: %s", s.SampleID)
		}
		if seenNodes[s.Node] {
			t.Fatalf("node sampled twice: %d", s.Node)
		}
		seenIDs[s.SampleID] = true
		seenNodes[s.Node] = true
	}
	for node := int64(1); node <= targets; node++ {
		if !seenNodes[node] {
			t.Fatalf("target %d missing from final samples", node)
		}
	}
	if final.Conclusion != ConclusionContaminated {
		t.Fatalf("conclusion = %s, want contaminated", final.Conclusion)
	}
}

// TestConcurrentRaceForLastTarget: many goroutines try to sample the single
// remaining target; exactly one wins and concludes the investigation.
func TestConcurrentRaceForLastTarget(t *testing.T) {
	store := NewStore()
	inv := store.Create(3, []Edge{{From: 0, To: 1}, {From: 0, To: 2}}, []int64{0}, fixedNow)
	if _, err := store.AddSample(inv.ID, Sample{SampleID: "first", Node: 1, Result: ResultClean}); err != nil {
		t.Fatalf("AddSample first: %v", err)
	}

	const racers = 16
	type outcome struct {
		inv *Investigation
		err error
	}
	results := make(chan outcome, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got, err := store.AddSample(inv.ID, Sample{
				SampleID: fmt.Sprintf("r-%d", i),
				Node:     2,
				Result:   ResultClean,
			})
			results <- outcome{got, err}
		}(i)
	}
	wg.Wait()
	close(results)

	var succeeded, completed int
	for out := range results {
		if out.err != nil {
			if !errors.Is(out.err, ErrNodeAlreadySampled) && !errors.Is(out.err, ErrCompleted) {
				t.Fatalf("unexpected error: %v", out.err)
			}
			continue
		}
		succeeded++
		if out.inv.Status != StatusCompleted {
			t.Fatalf("winning status = %s, want COMPLETED", out.inv.Status)
		}
		completed++
		if len(out.inv.Samples) != 2 {
			t.Fatalf("winning view has %d samples, want 2", len(out.inv.Samples))
		}
	}
	if succeeded != 1 || completed != 1 {
		t.Fatalf("succeeded = %d, completed = %d, want exactly 1 and 1", succeeded, completed)
	}
	if _, err := store.AddSample(inv.ID, Sample{SampleID: "late", Node: 2, Result: ResultClean}); !errors.Is(err, ErrCompleted) {
		t.Fatalf("write after race = %v, want ErrCompleted", err)
	}
}
