// Package investigation tracks post-storm contamination surveys of the water
// network. Each investigation snapshots the nodes, directed pipes and
// pollution inlets, derives the set of nodes reachable from the inlets (the
// sampling targets), and collects water samples until every target has been
// sampled.
package investigation

import (
	"errors"
	"maps"
	"slices"
	"strconv"
	"sync"
	"time"
)

// Status is the lifecycle state of an investigation. It only moves forward:
// PENDING -> IN_PROGRESS -> COMPLETED, and COMPLETED never regresses.
type Status string

const (
	// StatusPending: no sample has been registered yet.
	StatusPending Status = "PENDING"
	// StatusInProgress: some but not all targets have been sampled.
	StatusInProgress Status = "IN_PROGRESS"
	// StatusCompleted: every target has been sampled (terminal).
	StatusCompleted Status = "COMPLETED"
)

// Conclusion is the verdict of an investigation: "contaminated" as soon as
// any registered sample is contaminated, otherwise "clean".
type Conclusion string

const (
	ConclusionClean        Conclusion = "clean"
	ConclusionContaminated Conclusion = "contaminated"
)

// Result is the outcome of a single water sample.
type Result string

const (
	ResultClean        Result = "clean"
	ResultContaminated Result = "contaminated"
)

// Errors returned by the store; the HTTP layer maps them to status codes.
var (
	ErrNotFound           = errors.New("investigation not found")
	ErrCompleted          = errors.New("investigation is already completed")
	ErrDuplicateSampleID  = errors.New("sample_id is already registered")
	ErrNodeAlreadySampled = errors.New("node is already sampled")
	ErrNodeNotTarget      = errors.New("node is not a target of the investigation")
)

// Edge is one directed pipe of the network snapshot.
type Edge struct {
	From int64
	To   int64
}

// Sample is one registered water sample. SampleID is globally unique across
// all investigations.
type Sample struct {
	SampleID string
	Node     int64
	Result   Result
}

// Investigation is one contamination survey. Exported fields form the view
// returned to clients; the maps are bookkeeping for conflict checks.
type Investigation struct {
	ID         string
	Status     Status
	Conclusion Conclusion
	N          int64
	Edges      []Edge
	Inlets     []int64
	Targets    []int64
	Samples    []Sample
	CreatedAt  time.Time

	targetSet map[int64]struct{}
	sampled   map[int64]string // node -> sample_id
}

// Store keeps every investigation plus the global sample_id registry. All
// reads and mutations happen under one mutex, so registering a sample and
// advancing the lifecycle is a single critical section.
type Store struct {
	mu        sync.Mutex
	nextID    int64
	byID      map[string]*Investigation
	sampleIDs map[string]struct{}
}

// NewStore returns an empty in-memory store.
func NewStore() *Store {
	return &Store{
		byID:      make(map[string]*Investigation),
		sampleIDs: make(map[string]struct{}),
	}
}

// Create snapshots the network, derives the sampling targets and stores a new
// investigation. An investigation without targets is COMPLETED on creation.
// It returns a detached copy; the stored investigation is never aliased.
func (s *Store) Create(n int64, edges []Edge, inlets []int64, now time.Time) *Investigation {
	targets := reachableTargets(n, edges, inlets)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	inv := &Investigation{
		ID:         strconv.FormatInt(s.nextID, 10),
		Status:     StatusPending,
		Conclusion: ConclusionClean,
		N:          n,
		Edges:      slices.Clone(edges),
		Inlets:     dedupeSorted(inlets),
		Targets:    targets,
		Samples:    []Sample{},
		CreatedAt:  now,
		targetSet:  make(map[int64]struct{}, len(targets)),
		sampled:    make(map[int64]string),
	}
	for _, t := range targets {
		inv.targetSet[t] = struct{}{}
	}
	if len(targets) == 0 {
		inv.Status = StatusCompleted
	}
	s.byID[inv.ID] = inv
	return inv.clone()
}

// AddSample registers a sample and advances the lifecycle in the same
// critical section, so concurrent requests cannot double-count samples, lose
// one, or conclude an investigation twice. On any conflict nothing is
// recorded. It returns a detached copy of the updated investigation.
func (s *Store) AddSample(id string, sample Sample) (*Investigation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	inv, ok := s.byID[id]
	if !ok {
		return nil, ErrNotFound
	}
	if inv.Status == StatusCompleted {
		return nil, ErrCompleted
	}
	if _, dup := s.sampleIDs[sample.SampleID]; dup {
		return nil, ErrDuplicateSampleID
	}
	if _, dup := inv.sampled[sample.Node]; dup {
		return nil, ErrNodeAlreadySampled
	}
	if _, ok := inv.targetSet[sample.Node]; !ok {
		return nil, ErrNodeNotTarget
	}

	inv.Samples = append(inv.Samples, sample)
	inv.sampled[sample.Node] = sample.SampleID
	s.sampleIDs[sample.SampleID] = struct{}{}
	if sample.Result == ResultContaminated {
		inv.Conclusion = ConclusionContaminated
	}
	if len(inv.Samples) == len(inv.Targets) {
		// Terminal transition, reachable at most once: every later write
		// fails the StatusCompleted check above.
		inv.Status = StatusCompleted
	} else {
		inv.Status = StatusInProgress
	}
	return inv.clone(), nil
}

// reachableTargets returns the sorted set of nodes reachable from any inlet,
// excluding the inlets themselves. The traversal is iterative and marks every
// dequeued node, so cycles, self-loops and parallel edges cannot produce
// duplicates.
func reachableTargets(n int64, edges []Edge, inlets []int64) []int64 {
	adj := make([][]int64, n)
	for _, e := range edges {
		adj[e.From] = append(adj[e.From], e.To)
	}
	isInlet := make([]bool, n)
	visited := make([]bool, n)
	queue := make([]int64, 0, n)
	for _, v := range inlets {
		isInlet[v] = true
	}
	for _, v := range inlets {
		if !visited[v] {
			visited[v] = true
			queue = append(queue, v)
		}
	}
	targets := []int64{}
	for len(queue) > 0 {
		v := queue[0]
		queue = queue[1:]
		for _, w := range adj[v] {
			if visited[w] {
				continue
			}
			visited[w] = true
			queue = append(queue, w)
			if !isInlet[w] {
				targets = append(targets, w)
			}
		}
	}
	slices.Sort(targets)
	return targets
}

// clone returns a deep copy so callers never share mutable state with the
// store.
func (inv *Investigation) clone() *Investigation {
	c := *inv
	c.Edges = slices.Clone(inv.Edges)
	c.Inlets = slices.Clone(inv.Inlets)
	c.Targets = slices.Clone(inv.Targets)
	c.Samples = slices.Clone(inv.Samples)
	c.targetSet = maps.Clone(inv.targetSet)
	c.sampled = maps.Clone(inv.sampled)
	return &c
}

// dedupeSorted returns the sorted set of inlet node ids; duplicate inlets are
// stored once.
func dedupeSorted(values []int64) []int64 {
	out := slices.Clone(values)
	slices.Sort(out)
	out = slices.Compact(out)
	if out == nil {
		out = []int64{}
	}
	return out
}
