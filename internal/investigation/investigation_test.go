package investigation

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestResolveTargets(t *testing.T) {
	cases := []struct {
		name    string
		n       int
		edges   []Edge
		sources []int
		want    []int
	}{
		{
			// Edges: 0->1, 0->2 (parallel), 2->1 (self-loop at 1),
			// 1->3 cycle edge 3->1, 3->4. Inlets {0}.
			name: "parallel edges self loop and cycle do not duplicate",
			n:    5,
			edges: []Edge{
				{0, 1}, {0, 2}, {0, 2}, {2, 1}, {1, 1},
				{1, 3}, {3, 1}, {3, 4},
			},
			sources: []int{0},
			want:    []int{1, 2, 3, 4},
		},
		{
			name: "source reachable through a cycle is still excluded",
			n:    4,
			edges: []Edge{
				{0, 1}, {1, 2}, {2, 0}, {2, 3},
			},
			sources: []int{0},
			want:    []int{1, 2, 3}, // 0 excluded even though 2->0 loops back
		},
		{
			name: "multiple sources and shared reachable nodes",
			n:    6,
			edges: []Edge{
				{0, 2}, {1, 2}, {2, 3}, {2, 4}, {4, 5},
			},
			sources: []int{0, 1},
			want:    []int{2, 3, 4, 5},
		},
		{
			name:    "disconnected network yields no targets",
			n:       4,
			edges:   []Edge{{2, 3}, {3, 2}},
			sources: []int{0},
			want:    []int{},
		},
		{
			name:    "only self-loops at the source yield no targets",
			n:       2,
			edges:   []Edge{{0, 0}, {0, 0}},
			sources: []int{0},
			want:    []int{},
		},
		{
			name:    "outgoing edges to another source do not make it a target",
			n:       3,
			edges:   []Edge{{0, 1}, {1, 2}},
			sources: []int{0, 1},
			want:    []int{2},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveTargets(tc.n, tc.edges, tc.sources)
			if len(got) != len(tc.want) {
				t.Fatalf("targets = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("targets = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestCreateWithoutTargetsCompletesClean(t *testing.T) {
	repo := NewRepository()
	view, err := repo.Create(3, []Edge{{1, 2}}, []int{0})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if view.Status != StatusCompleted {
		t.Fatalf("status = %s, want COMPLETED", view.Status)
	}
	if view.Result != ResultClean {
		t.Fatalf("result = %q, want clean", view.Result)
	}
	if len(view.Targets) != 0 {
		t.Fatalf("targets = %v, want empty", view.Targets)
	}
	if _, err := repo.AddSample(view.ID, "s1", 2, "clean"); !errors.Is(err, ErrCompleted) {
		t.Fatalf("AddSample after completion error = %v, want ErrCompleted", err)
	}
}

func TestLifecycleClean(t *testing.T) {
	repo := NewRepository()
	view, err := repo.Create(4, []Edge{{0, 1}, {1, 2}, {2, 3}}, []int{0})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if view.Status != StatusPending || view.Result != "" || len(view.Samples) != 0 {
		t.Fatalf("initial view = %+v, want PENDING/no result/no samples", view)
	}

	view, err = repo.AddSample(view.ID, "s-1", 1, "clean")
	if err != nil {
		t.Fatalf("AddSample: %v", err)
	}
	if view.Status != StatusInProgress || view.Result != "" || len(view.Samples) != 1 {
		t.Fatalf("after one sample view = %+v, want IN_PROGRESS", view)
	}

	view, err = repo.AddSample(view.ID, "s-2", 2, "clean")
	if err != nil {
		t.Fatalf("AddSample: %v", err)
	}
	if view.Status != StatusInProgress {
		t.Fatalf("status = %s, want IN_PROGRESS", view.Status)
	}

	view, err = repo.AddSample(view.ID, "s-3", 3, "clean")
	if err != nil {
		t.Fatalf("AddSample: %v", err)
	}
	if view.Status != StatusCompleted || view.Result != ResultClean {
		t.Fatalf("final view = %+v, want COMPLETED/clean", view)
	}
	if len(view.Samples) != 3 {
		t.Fatalf("samples = %v, want 3", view.Samples)
	}
	// Samples are presented in ascending node order.
	for i, s := range view.Samples {
		if s.Node != i+1 {
			t.Fatalf("samples not ordered by node: %v", view.Samples)
		}
	}
}

func TestLifecycleContaminated(t *testing.T) {
	repo := NewRepository()
	view, _ := repo.Create(3, []Edge{{0, 1}, {1, 2}}, []int{0})

	view, err := repo.AddSample(view.ID, "c1", 2, "contaminated")
	if err != nil {
		t.Fatalf("AddSample: %v", err)
	}
	if view.Status != StatusInProgress || view.Result != "" {
		t.Fatalf("partial view = %+v, want open IN_PROGRESS", view)
	}
	view, err = repo.AddSample(view.ID, "c2", 1, "clean")
	if err != nil {
		t.Fatalf("AddSample: %v", err)
	}
	if view.Status != StatusCompleted || view.Result != ResultContaminated {
		t.Fatalf("final view = %+v, want COMPLETED/contaminated", view)
	}
}

func TestSampleConflicts(t *testing.T) {
	repo := NewRepository()
	inv, _ := repo.Create(3, []Edge{{0, 1}, {1, 2}}, []int{0})

	if _, err := repo.AddSample("does-not-exist", "x", 1, "clean"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown investigation: %v, want ErrNotFound", err)
	}
	if _, err := repo.AddSample(inv.ID, "x", 3, "clean"); !errors.Is(err, ErrNodeOutOfRange) {
		t.Fatalf("node out of range: %v, want ErrNodeOutOfRange", err)
	}
	if _, err := repo.AddSample(inv.ID, "x", 0, "clean"); !errors.Is(err, ErrNodeNotTarget) {
		t.Fatalf("source node: %v, want ErrNodeNotTarget", err)
	}

	if _, err := repo.AddSample(inv.ID, "dup-id", 1, "clean"); err != nil {
		t.Fatalf("first AddSample: %v", err)
	}
	if _, err := repo.AddSample(inv.ID, "dup-id", 2, "clean"); !errors.Is(err, ErrDuplicateSampleID) {
		t.Fatalf("same sample_id: %v, want ErrDuplicateSampleID", err)
	}
	if _, err := repo.AddSample(inv.ID, "again", 1, "clean"); !errors.Is(err, ErrDuplicateNode) {
		t.Fatalf("same node: %v, want ErrDuplicateNode", err)
	}
}

func TestSampleIDIsGloballyUnique(t *testing.T) {
	repo := NewRepository()
	a, _ := repo.Create(2, []Edge{{0, 1}}, []int{0})
	b, _ := repo.Create(2, []Edge{{0, 1}}, []int{0})
	if _, err := repo.AddSample(a.ID, "global-id", 1, "clean"); err != nil {
		t.Fatalf("AddSample a: %v", err)
	}
	if _, err := repo.AddSample(b.ID, "global-id", 1, "clean"); !errors.Is(err, ErrDuplicateSampleID) {
		t.Fatalf("reused sample_id across investigations: %v, want ErrDuplicateSampleID", err)
	}
}

// TestConcurrentCompletionFillsEachTargetOnce hammers one open investigation
// with one request per target per worker. Exactly one registration per target
// may survive, the investigation must close exactly once and the closed
// state must never reopen.
func TestConcurrentCompletionFillsEachTargetOnce(t *testing.T) {
	const targets = 50
	repo := NewRepository()
	edges := make([]Edge, 0, targets)
	for v := 1; v <= targets; v++ {
		edges = append(edges, Edge{0, v})
	}
	inv, err := repo.Create(targets+1, edges, []int{0})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	const workers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	var accepted, completedViews, inProgressViews int
	start := make(chan struct{})
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for node := 1; node <= targets; node++ {
				view, err := repo.AddSample(inv.ID,
					fmt.Sprintf("w%d-n%d", w, node), node, "clean")
				if err == nil {
					mu.Lock()
					accepted++
					if view.Status == StatusCompleted {
						completedViews++
					} else {
						inProgressViews++
					}
					mu.Unlock()
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	if accepted != targets {
		t.Fatalf("accepted samples = %d, want exactly %d", accepted, targets)
	}
	if completedViews != 1 {
		t.Fatalf("COMPLETED responses = %d, want exactly 1", completedViews)
	}
	if inProgressViews != targets-1 {
		t.Fatalf("IN_PROGRESS responses = %d, want %d", inProgressViews, targets-1)
	}

	final, err := repo.Get(inv.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.Status != StatusCompleted || final.Result != ResultClean {
		t.Fatalf("final = %+v, want COMPLETED/clean", final)
	}
	if len(final.Samples) != targets {
		t.Fatalf("stored samples = %d, want %d", len(final.Samples), targets)
	}
	seen := make(map[int]bool, targets)
	for _, s := range final.Samples {
		if seen[s.Node] {
			t.Fatalf("node %d recorded more than once", s.Node)
		}
		seen[s.Node] = true
	}

	// Every post-completion write is rejected, including a fresh id/node.
	if _, err := repo.AddSample(inv.ID, "post-close", 1, "contaminated"); !errors.Is(err, ErrCompleted) {
		t.Fatalf("write after completion: %v, want ErrCompleted", err)
	}
	final, _ = repo.Get(inv.ID)
	if final.Result != ResultClean {
		t.Fatalf("result changed to %q after terminal write; terminal state must not regress", final.Result)
	}
}

func TestCreateSnapshotsInputs(t *testing.T) {
	repo := NewRepository()
	edges := []Edge{{0, 1}, {1, 2}}
	sources := []int{0}
	view, _ := repo.Create(3, edges, sources)

	// Mutate the caller's slices after creation.
	edges[0] = Edge{2, 0}
	sources[0] = 2

	again, _ := repo.Get(view.ID)
	if again.Edges[0] != (Edge{0, 1}) || again.Sources[0] != 0 {
		t.Fatalf("snapshot was mutated by caller: edges=%v sources=%v", again.Edges, again.Sources)
	}

	// The returned view must be independent too.
	again.Targets[0] = 99
	again2, _ := repo.Get(view.ID)
	if again2.Targets[0] != 1 {
		t.Fatalf("repository state mutated through returned view: %v", again2.Targets)
	}
}
