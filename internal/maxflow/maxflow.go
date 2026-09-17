// Package maxflow computes the maximum flow (equivalently, the minimum
// s-t cut capacity) of a directed network with 64-bit integer capacities.
//
// The implementation is a self-contained highest-label push-relabel
// algorithm with the gap heuristic: nodes hold a height label, excess flow
// is pushed downhill along residual arcs, and once a height level empties
// every node above it is known to be cut off from the sink. No external
// solver is used.
package maxflow

// edge is one residual arc. The reverse arc lives at adj[to][rev].
type edge struct {
	to  int
	rev int
	cap int64
}

// Graph is a directed flow network. Nodes are numbered 0..n-1.
type Graph struct {
	adj [][]edge
}

// New returns an empty flow network with n nodes.
func New(n int) *Graph {
	return &Graph{adj: make([][]edge, n)}
}

// AddEdge appends a directed arc u -> v with the given capacity.
// Parallel arcs are allowed; each carries (and is charged) its own capacity.
func (g *Graph) AddEdge(u, v int, capacity int64) {
	g.adj[u] = append(g.adj[u], edge{to: v, rev: len(g.adj[v]), cap: capacity})
	g.adj[v] = append(g.adj[v], edge{to: u, rev: len(g.adj[u]) - 1, cap: 0})
}

// MaxFlow returns the value of a maximum s-t flow. By the max-flow min-cut
// theorem this equals the minimum capacity of an s-t cut, i.e. the cheapest
// set of arcs whose removal disconnects t from s.
func (g *Graph) MaxFlow(s, t int) int64 {
	n := len(g.adj)
	if n == 0 || s == t {
		return 0
	}

	excess := make([]int64, n)
	height := make([]int, n)
	cur := make([]int, n)
	// count[h] = number of nodes at height h, for the gap heuristic.
	// Heights never exceed 2n (a node with excess always has a residual
	// path back to s, which sits at height n).
	count := make([]int, 2*n+2)
	// buckets[h] = active nodes (excess > 0) at height h, for h < n. Nodes
	// at height >= n cannot reach the sink at all, so they are never
	// scheduled again; their excess is irrelevant to the flow value.
	buckets := make([][]int, n)

	height[s] = n
	count[0] = n - 1
	count[n] = 1

	// Saturate every arc out of s, turning its neighbours active.
	for i := range g.adj[s] {
		e := &g.adj[s][i]
		if e.cap > 0 {
			g.adj[e.to][e.rev].cap += e.cap
			excess[e.to] += e.cap
			excess[s] -= e.cap
			e.cap = 0
		}
	}
	maxB := 0
	for v := 0; v < n; v++ {
		if v != s && v != t && excess[v] > 0 {
			buckets[0] = append(buckets[0], v)
		}
	}

	for maxB >= 0 {
		if len(buckets[maxB]) == 0 {
			maxB--
			continue
		}
		v := buckets[maxB][len(buckets[maxB])-1]
		buckets[maxB] = buckets[maxB][:len(buckets[maxB])-1]
		if height[v] != maxB {
			continue // stale entry: v was parked at height n by a gap
		}

		// Discharge v until its excess is gone or it can no longer reach t.
		for excess[v] > 0 && height[v] < n {
			if cur[v] < len(g.adj[v]) {
				e := &g.adj[v][cur[v]]
				if e.cap > 0 && height[v] == height[e.to]+1 {
					f := e.cap
					if excess[v] < f {
						f = excess[v]
					}
					e.cap -= f
					g.adj[e.to][e.rev].cap += f
					if e.to != s && e.to != t && excess[e.to] == 0 {
						if h := height[e.to]; h < n {
							buckets[h] = append(buckets[h], e.to)
							if h > maxB {
								maxB = h
							}
						}
					}
					excess[e.to] += f
					excess[v] -= f
				} else {
					cur[v]++
				}
				continue
			}

			// Relabel: rise one above the lowest residual neighbour. A node
			// holding excess always has at least one residual arc (the
			// reverse of whatever filled it), so low is always found.
			old := height[v]
			low := 2*n + 1
			for _, e := range g.adj[v] {
				if e.cap > 0 && height[e.to] < low {
					low = height[e.to]
				}
			}
			height[v] = low + 1
			cur[v] = 0
			count[old]--
			count[height[v]]++
			if count[old] == 0 && old < n {
				// Gap: the level below is empty, so nothing above it can
				// reach the sink any more. Park those nodes at height n
				// (this may include v itself, which is correct: it is
				// above the gap too).
				for u := 0; u < n; u++ {
					if u != s && height[u] > old && height[u] < n {
						count[height[u]]--
						height[u] = n
						count[n]++
					}
				}
			}
		}
	}
	return excess[t]
}
