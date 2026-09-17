package maxflow

import (
	"math/rand"
	"testing"
)

type arc struct {
	u, v int
	cap  int64
}

func buildGraph(n int, arcs []arc) *Graph {
	g := New(n)
	for _, a := range arcs {
		g.AddEdge(a.u, a.v, a.cap)
	}
	return g
}

func TestMaxFlowKnownCases(t *testing.T) {
	cases := []struct {
		name string
		n    int
		arcs []arc
		s, t int
		want int64
	}{
		{"single arc", 2, []arc{{0, 1, 7}}, 0, 1, 7},
		{"parallel arcs are summed", 2, []arc{{0, 1, 3}, {0, 1, 4}}, 0, 1, 7},
		{"parallel arcs cheaper than downstream", 3,
			[]arc{{0, 1, 3}, {0, 1, 4}, {1, 2, 10}}, 0, 2, 7},
		{"self loops carry no flow", 3,
			[]arc{{0, 0, 100}, {0, 1, 3}, {1, 1, 100}, {1, 2, 10}}, 0, 2, 3},
		{"diamond", 4,
			[]arc{{0, 1, 1}, {0, 2, 1}, {1, 3, 1}, {2, 3, 1}}, 0, 3, 2},
		{"no path", 4, []arc{{0, 1, 9}, {2, 3, 9}}, 0, 3, 0},
		{"backward arc does not count", 2, []arc{{1, 0, 5}}, 0, 1, 0},
		{"bottleneck in the middle", 4,
			[]arc{{0, 1, 100}, {1, 2, 6}, {2, 3, 100}}, 0, 3, 6},
		{"64 bit capacities", 2,
			[]arc{{0, 1, 1_000_000_000}, {0, 1, 1_000_000_000}, {0, 1, 1_000_000_000}},
			0, 1, 3_000_000_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildGraph(tc.n, tc.arcs).MaxFlow(tc.s, tc.t); got != tc.want {
				t.Fatalf("MaxFlow = %d, want %d", got, tc.want)
			}
		})
	}
}

// edmondsKarp is an independent, deliberately simple reference
// implementation used only to cross-check Dinic's results in tests.
func edmondsKarp(n int, arcs []arc, s, t int) int64 {
	if s == t {
		return 0
	}
	res := make([][]int64, n)
	for i := range res {
		res[i] = make([]int64, n)
	}
	for _, a := range arcs {
		res[a.u][a.v] += a.cap
	}
	var flow int64
	for {
		prev := make([]int, n)
		for i := range prev {
			prev[i] = -1
		}
		prev[s] = s
		queue := []int{s}
		for head := 0; head < len(queue) && prev[t] < 0; head++ {
			v := queue[head]
			for w := 0; w < n; w++ {
				if res[v][w] > 0 && prev[w] < 0 {
					prev[w] = v
					queue = append(queue, w)
				}
			}
		}
		if prev[t] < 0 {
			return flow
		}
		// bottleneck on the discovered path
		b := res[prev[t]][t]
		for v := t; v != s; v = prev[v] {
			if res[prev[v]][v] < b {
				b = res[prev[v]][v]
			}
		}
		for v := t; v != s; v = prev[v] {
			res[prev[v]][v] -= b
			res[v][prev[v]] += b
		}
		flow += b
	}
}

func TestMaxFlowMatchesEdmondsKarp(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for trial := 0; trial < 300; trial++ {
		n := 2 + rng.Intn(11)
		m := rng.Intn(41)
		arcs := make([]arc, 0, m)
		for i := 0; i < m; i++ {
			arcs = append(arcs, arc{rng.Intn(n), rng.Intn(n), 1 + rng.Int63n(1000)})
		}
		s, tt := rng.Intn(n), rng.Intn(n)
		for tt == s {
			tt = rng.Intn(n)
		}
		got := buildGraph(n, arcs).MaxFlow(s, tt)
		want := edmondsKarp(n, arcs, s, tt)
		if got != want {
			t.Fatalf("trial %d: Dinic = %d, Edmonds-Karp = %d (n=%d arcs=%v s=%d t=%d)",
				trial, got, want, n, arcs, s, tt)
		}
	}
}

// TestMaxFlowChainAtScale exercises the upper size limits (20k nodes,
// ~100k arcs) on a chain whose minimum cut is known exactly: the cheapest
// hop, i.e. the minimum over hops of the sum of its parallel arc costs.
func TestMaxFlowChainAtScale(t *testing.T) {
	const n = 20000
	rng := rand.New(rand.NewSource(7))
	arcs := make([]arc, 0, 5*(n-1))
	want := int64(-1)
	for hop := 0; hop+1 < n; hop++ {
		var hopSum int64
		for k := 0; k < 5; k++ {
			c := 1 + rng.Int63n(1_000_000_000)
			arcs = append(arcs, arc{hop, hop + 1, c})
			hopSum += c
		}
		if want < 0 || hopSum < want {
			want = hopSum
		}
	}
	if got := buildGraph(n, arcs).MaxFlow(0, n-1); got != want {
		t.Fatalf("MaxFlow = %d, want %d", got, want)
	}
}
