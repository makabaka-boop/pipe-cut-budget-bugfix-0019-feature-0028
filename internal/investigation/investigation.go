// Package investigation implements the post-storm pollution sampling domain:
// a survey snapshots a water-pipe network (nodes, directed pipes and the
// pollution inlets), derives the set of nodes that pollution can reach from
// any inlet (the inlets themselves excluded), and then tracks one water
// sample per target node until every target has been sampled.
//
// The lifecycle is monotonic: PENDING (no samples yet) -> IN_PROGRESS (some
// targets sampled) -> COMPLETED (all targets sampled, or no targets at
// creation). A completed investigation is closed once and never reopens.
// The conclusion is "clean" unless at least one registered sample is
// "contaminated".
package investigation

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Limits shared with the HTTP layer.
const (
	MinNodes = 2
	MaxNodes = 20000
	MaxEdges = 100000
)

// Status is the monotonic lifecycle state of an investigation.
type Status string

const (
	// StatusPending means no sample has been registered yet.
	StatusPending Status = "PENDING"
	// StatusInProgress means at least one, but not all, targets are sampled.
	StatusInProgress Status = "IN_PROGRESS"
	// StatusCompleted means every target is sampled (or there were none).
	// This state never regresses.
	StatusCompleted Status = "COMPLETED"
)

// Result is the final conclusion. It is the zero value until completion.
type Result string

const (
	ResultClean        Result = "clean"
	ResultContaminated Result = "contaminated"
)

// Edge is one directed pipe stored in the creation snapshot.
type Edge struct {
	From int
	To   int
}

// Sample is one registered water sample.
type Sample struct {
	SampleID string
	Node     int
	Verdict  string // "clean" or "contaminated"
}

// View is the complete, serialization-independent representation of an
// investigation. The repository only hands out deep copies.
type View struct {
	ID      string
	N       int
	Edges   []Edge
	Sources []int
	Targets []int
	Status  Status
	Result  Result // "" while the investigation is still open
	Samples []Sample
}

// Domain errors. The HTTP layer maps these onto 404/409/422.
var (
	// ErrNotFound is returned when no investigation carries the given id.
	ErrNotFound = errors.New("investigation not found")
	// ErrNodeOutOfRange is returned for a sample at a node that is not part
	// of the snapshotted network. NodeOutOfRangeError carries the network
	// size and offending node for error messages.
	ErrNodeOutOfRange = errors.New("sample node is outside the network")
	// ErrCompleted is returned when a sample is filed against an
	// investigation that has already reached COMPLETED.
	ErrCompleted = errors.New("investigation already completed")
	// ErrDuplicateSampleID is returned when the sample id was already used,
	// in this or any other investigation.
	ErrDuplicateSampleID = errors.New("sample_id already used")
	// ErrNodeNotTarget is returned when the node is in the network but is
	// not a pollution-reachable target.
	ErrNodeNotTarget = errors.New("node is not a sampling target")
	// ErrDuplicateNode is returned when the target node was already sampled.
	ErrDuplicateNode = errors.New("node already sampled")
)

// NodeOutOfRangeError wraps ErrNodeOutOfRange with the network bounds of the
// investigation that rejected the sample.
type NodeOutOfRangeError struct {
	N    int
	Node int
}

func (e *NodeOutOfRangeError) Error() string {
	return fmt.Sprintf("%s: node %d is outside [0, %d)", ErrNodeOutOfRange, e.Node, e.N)
}

// Unwrap exposes the sentinel so errors.Is(err, ErrNodeOutOfRange) works.
func (e *NodeOutOfRangeError) Unwrap() error { return ErrNodeOutOfRange }

// ResolveTargets returns the distinct nodes, in ascending order, reachable
// from any source following directed edges, excluding the source nodes
// themselves. The traversal is iterative (explicit frontier) so a network
// with tens of thousands of nodes cannot blow the call stack, and a visited
// set makes cycles, self-loops and parallel edges collapse to one visit
// each: a reachable node is returned exactly once.
func ResolveTargets(n int, edges []Edge, sources []int) []int {
	adj := make([][]int, n)
	for _, e := range edges {
		adj[e.From] = append(adj[e.From], e.To)
	}

	reachable := make([]bool, n)
	frontier := make([]int, 0, len(sources))
	for _, s := range sources {
		// Mark the inlets themselves as visited: they must never be queued
		// as targets, even via another inlet or through a cycle.
		if !reachable[s] {
			reachable[s] = true
			frontier = append(frontier, s)
		}
	}

	for head := 0; head < len(frontier); head++ {
		u := frontier[head]
		for _, v := range adj[u] {
			if !reachable[v] {
				reachable[v] = true
				frontier = append(frontier, v)
			}
		}
	}

	targets := make([]int, 0)
	for v := 0; v < n; v++ {
		if reachable[v] && !isSource(v, sources) {
			targets = append(targets, v)
		}
	}
	return targets
}

func isSource(v int, sources []int) bool {
	for _, s := range sources {
		if s == v {
			return true
		}
	}
	return false
}

// investigation is the stored aggregate. A single Repository mutex guards
// every field of every stored investigation, so sample registration and the
// state/conclusion transition happen atomically.
type investigation struct {
	n            int
	edges        []Edge
	sources      []int
	targets      []int
	targetSet    map[int]struct{}
	sampledNodes map[int]int // target node -> index in samples
	samples      []Sample
	status       Status
	result       Result
	contaminated bool
}

// Repository is the in-memory investigation store. Its zero value is ready
// for use; it is safe for concurrent callers.
type Repository struct {
	mu        sync.Mutex
	items     map[string]*investigation
	sampleIDs map[string]struct{}
}

// NewRepository returns an empty repository.
func NewRepository() *Repository {
	return &Repository{
		items:     make(map[string]*investigation),
		sampleIDs: make(map[string]struct{}),
	}
}

// Create snapshots the network, resolves the target set and registers a new
// investigation. With no reachable target the investigation is created
// already COMPLETED with conclusion "clean"; otherwise it starts PENDING.
// The inputs are assumed to have passed HTTP validation (bounds and unique
// sources).
func (r *Repository) Create(n int, edges []Edge, sources []int) (View, error) {
	id, err := newID()
	if err != nil {
		return View{}, fmt.Errorf("generate investigation id: %w", err)
	}

	// Snapshot the caller's slices: later mutation by the HTTP layer must
	// not change the stored graph.
	edgeSnap := make([]Edge, len(edges))
	copy(edgeSnap, edges)
	sourceSnap := make([]int, len(sources))
	copy(sourceSnap, sources)
	targets := ResolveTargets(n, edges, sources)

	agg := &investigation{
		n:            n,
		edges:        edgeSnap,
		sources:      sourceSnap,
		targets:      targets,
		targetSet:    make(map[int]struct{}, len(targets)),
		sampledNodes: make(map[int]int, len(targets)),
		samples:      make([]Sample, 0, len(targets)),
		status:       StatusPending,
	}
	for _, t := range targets {
		agg.targetSet[t] = struct{}{}
	}
	if len(targets) == 0 {
		// Nothing to sample: creation is completion.
		agg.status = StatusCompleted
		agg.result = ResultClean
	}

	r.mu.Lock()
	r.items[id] = agg
	r.mu.Unlock()

	return agg.snapshot(id), nil
}

// Get returns a deep copy of the investigation, or ErrNotFound.
func (r *Repository) Get(id string) (View, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	agg, ok := r.items[id]
	if !ok {
		return View{}, ErrNotFound
	}
	return agg.snapshot(id), nil
}

// AddSample registers a sample and advances the lifecycle in the same
// critical section. Concurrent callers racing to fill the last open target
// serialize here, so every target node is recorded exactly once and the
// investigation is completed exactly once.
func (r *Repository) AddSample(id, sampleID string, node int, verdict string) (View, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	agg, ok := r.items[id]
	if !ok {
		return View{}, ErrNotFound
	}
	if node < 0 || node >= agg.n {
		return View{}, &NodeOutOfRangeError{N: agg.n, Node: node}
	}
	// Terminal state is checked before any write: the closed investigation
	// never regresses and no stray sample is attached to it.
	if agg.status == StatusCompleted {
		return View{}, ErrCompleted
	}
	// Sample ids are globally unique across every investigation.
	if _, dup := r.sampleIDs[sampleID]; dup {
		return View{}, ErrDuplicateSampleID
	}
	if _, isTarget := agg.targetSet[node]; !isTarget {
		return View{}, ErrNodeNotTarget
	}
	if _, seen := agg.sampledNodes[node]; seen {
		return View{}, ErrDuplicateNode
	}

	r.sampleIDs[sampleID] = struct{}{}
	agg.samples = append(agg.samples, Sample{SampleID: sampleID, Node: node, Verdict: verdict})
	agg.sampledNodes[node] = len(agg.samples) - 1
	if verdict == string(ResultContaminated) {
		agg.contaminated = true
	}

	if len(agg.samples) == len(agg.targets) {
		// The last open target has just been filled: close exactly once,
		// under the same lock that admitted this sample.
		agg.status = StatusCompleted
		if agg.contaminated {
			agg.result = ResultContaminated
		} else {
			agg.result = ResultClean
		}
	} else {
		agg.status = StatusInProgress
	}

	return agg.snapshot(id), nil
}

// snapshot builds a deep copy of the aggregate so callers cannot mutate
// repository state. Targets are stored ascending; samples are returned in
// ascending node order for a stable view.
func (a *investigation) snapshot(id string) View {
	edges := make([]Edge, len(a.edges))
	copy(edges, a.edges)
	sources := make([]int, len(a.sources))
	copy(sources, a.sources)
	targets := make([]int, len(a.targets))
	copy(targets, a.targets)
	samples := make([]Sample, len(a.samples))
	copy(samples, a.samples)
	sortSamplesByNode(samples, a.targets)

	return View{
		ID:      id,
		N:       a.n,
		Edges:   edges,
		Sources: sources,
		Targets: targets,
		Status:  a.status,
		Result:  a.result,
		Samples: samples,
	}
}

// newID returns an opaque, globally unique investigation identifier.
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "inv_" + hex.EncodeToString(b[:]), nil
}

// sortSamplesByNode orders samples by ascending target node. Insertion order
// depends on which concurrent request wins the lock, so the view is sorted
// to stay deterministic.
func sortSamplesByNode(samples []Sample, targets []int) {
	rank := make(map[int]int, len(targets))
	for i, t := range targets {
		rank[t] = i
	}
	sort.Slice(samples, func(i, j int) bool {
		return rank[samples[i].Node] < rank[samples[j].Node]
	})
}
