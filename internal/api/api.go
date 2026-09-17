// Package api exposes the HTTP interface of the raincut service: given a
// directed graph of pipes, a set of pollution inlets (sources) and a set of
// water intakes (sinks), it computes the minimum total cost of pipes that
// must be shut so no source can reach any sink.
package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"raincut/internal/maxflow"
)

// Input limits. Any violation is rejected with HTTP 422 before the graph
// reaches the solver.
const (
	minNodes = 2
	maxNodes = 20000
	maxEdges = 100000
	minCost  = 1
	maxCost  = 1_000_000_000

	maxBodyBytes = 64 << 20 // 100k edges serialize to a few MB
)

// Edge is one directed pipe: from -> to with a shutdown cost.
type Edge struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
	Cost int64 `json:"cost"`
}

// Request is the JSON document accepted by POST /minimum-shutdown-cost.
// Nodes are numbered 0..n-1.
type Request struct {
	N       int64   `json:"n"`
	Edges   []Edge  `json:"edges"`
	Sources []int64 `json:"sources"`
	Sinks   []int64 `json:"sinks"`
}

// Response is the JSON document returned on success.
type Response struct {
	MinimumShutdownCost int64 `json:"minimum_shutdown_cost"`
}

// errorResponse is the stable error envelope returned for every rejected
// request: {"error": {"code": ..., "message": ...}}.
type errorResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// New returns the root HTTP handler of the service.
func New() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /minimum-shutdown-cost", handleMinimumShutdownCost)
	mux.HandleFunc("POST /investigations", handleCreateInvestigation)
	mux.HandleFunc("POST /investigations/{id}/samples", handleAddSample)
	mux.HandleFunc("GET /healthz", handleHealthz)
	return mux
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func handleMinimumShutdownCost(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var req Request
	if err := dec.Decode(&req); err != nil {
		writeError(w, "invalid_json",
			"body must be one JSON object with fields n, edges, sources, sinks: "+err.Error())
		return
	}
	if dec.More() {
		writeError(w, "invalid_json", "unexpected data after the JSON document")
		return
	}
	if msg := validate(&req); msg != "" {
		writeError(w, "invalid_graph", msg)
		return
	}

	writeJSON(w, http.StatusOK, Response{MinimumShutdownCost: solve(&req)})
}

// validate checks every input constraint and returns a human-readable
// message for the first violation, or "" when the request is solvable.
func validate(req *Request) string {
	if req.N < minNodes || req.N > maxNodes {
		return fmt.Sprintf("n must be between %d and %d, got %d", minNodes, maxNodes, req.N)
	}
	n := req.N
	if len(req.Edges) > maxEdges {
		return fmt.Sprintf("edges must contain at most %d entries, got %d", maxEdges, len(req.Edges))
	}
	for i, e := range req.Edges {
		if e.From < 0 || e.From >= n {
			return fmt.Sprintf("edges[%d].from must be a node id in [0, %d], got %d", i, n-1, e.From)
		}
		if e.To < 0 || e.To >= n {
			return fmt.Sprintf("edges[%d].to must be a node id in [0, %d], got %d", i, n-1, e.To)
		}
		if e.Cost < minCost || e.Cost > maxCost {
			return fmt.Sprintf("edges[%d].cost must be an integer in [%d, %d], got %d", i, minCost, maxCost, e.Cost)
		}
	}
	if len(req.Sources) == 0 {
		return "sources must be a non-empty array of node ids"
	}
	if len(req.Sinks) == 0 {
		return "sinks must be a non-empty array of node ids"
	}
	role := make([]uint8, n) // bit 1: listed as source, bit 2: listed as sink
	for i, v := range req.Sources {
		if v < 0 || v >= n {
			return fmt.Sprintf("sources[%d] must be a node id in [0, %d], got %d", i, n-1, v)
		}
		role[v] |= 1
	}
	for i, v := range req.Sinks {
		if v < 0 || v >= n {
			return fmt.Sprintf("sinks[%d] must be a node id in [0, %d], got %d", i, n-1, v)
		}
		role[v] |= 2
	}
	for v := int64(0); v < n; v++ {
		if role[v] == 3 {
			return fmt.Sprintf("node %d appears in both sources and sinks; the two groups must be disjoint", v)
		}
	}
	return ""
}

// solve reduces the problem to a minimum s-t cut: a super source feeds
// every pollution inlet and every intake drains into a super sink, both
// through arcs that cost one more than all real pipes combined. Such arcs
// can therefore never belong to a minimum cut, so the max-flow value is
// exactly the cheapest set of real pipes to shut.
func solve(req *Request) int64 {
	n := int(req.N)
	g := maxflow.New(n + 2)

	var total int64
	for _, e := range req.Edges {
		total += e.Cost
		if e.From == e.To {
			continue // a self-loop can never cross a cut
		}
		g.AddEdge(int(e.From), int(e.To), e.Cost)
	}

	link := total + 1
	superSource, superSink := n, n+1
	for _, v := range req.Sources {
		g.AddEdge(superSource, int(v), link)
	}
	for _, v := range req.Sinks {
		g.AddEdge(int(v), superSink, link)
	}
	return g.MaxFlow(superSource, superSink)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, code, message string) {
	writeErrorStatus(w, http.StatusUnprocessableEntity, code, message)
}

// writeErrorStatus emits the same stable {"error": {"code", "message"}}
// envelope as writeError, but with an arbitrary status (404/409 for the
// investigation endpoints).
func writeErrorStatus(w http.ResponseWriter, status int, code, message string) {
	var body errorResponse
	body.Error.Code = code
	body.Error.Message = message
	writeJSON(w, status, body)
}
