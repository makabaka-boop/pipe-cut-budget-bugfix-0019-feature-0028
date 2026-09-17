package maxflow

import (
	"math/rand"
	"testing"
)

// Benchmarks run only on demand: go test -bench . ./internal/maxflow

func benchRandom(b *testing.B) {
	const n, m = 20000, 100000
	rng := rand.New(rand.NewSource(1))
	g := New(n)
	for i := 0; i < m; i++ {
		g.AddEdge(rng.Intn(n), rng.Intn(n), 1+rng.Int63n(1_000_000_000))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g.MaxFlow(0, n-1)
	}
}

func benchChain(b *testing.B) {
	const n = 20000
	rng := rand.New(rand.NewSource(2))
	g := New(n)
	for hop := 0; hop+1 < n; hop++ {
		for k := 0; k < 5; k++ {
			g.AddEdge(hop, hop+1, 1+rng.Int63n(1_000_000_000))
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g.MaxFlow(0, n-1)
	}
}

func benchGrid(b *testing.B) {
	const side = 141 // 141*141 = 19881 nodes
	rng := rand.New(rand.NewSource(3))
	g := New(side * side)
	at := func(r, c int) int { return r*side + c }
	edges := 0
	for r := 0; r < side && edges < 100000; r++ {
		for c := 0; c < side && edges < 100000; c++ {
			if r+1 < side {
				g.AddEdge(at(r, c), at(r+1, c), 1+rng.Int63n(1_000_000_000))
				edges++
			}
			if c+1 < side && edges < 100000 {
				g.AddEdge(at(r, c), at(r, c+1), 1+rng.Int63n(1_000_000_000))
				edges++
			}
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g.MaxFlow(at(0, 0), at(side-1, side-1))
	}
}

func BenchmarkMaxFlowRandom(b *testing.B) { benchRandom(b) }
func BenchmarkMaxFlowChain(b *testing.B)  { benchChain(b) }
func BenchmarkMaxFlowGrid(b *testing.B)   { benchGrid(b) }
