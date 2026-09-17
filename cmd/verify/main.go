// Command verify is the one-shot acceptance client. It assumes the Go unit
// tests have already been run by the caller (the Docker Compose "verify"
// service runs `go test ./...` first), then exercises a live API server:
// exact shutdown costs for parallel edges, self-loops and multi-source /
// multi-sink graphs, a stable 422 error envelope for invalid graphs, and a
// 20000-node / 100000-edge graph whose exact cost must be returned within a
// 10 second request timeout.
//
// The API base URL is read from API_URL (default http://localhost:8080).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"time"
)

type edge struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
	Cost int64 `json:"cost"`
}

type graphRequest struct {
	N       int64   `json:"n"`
	Edges   []edge  `json:"edges"`
	Sources []int64 `json:"sources"`
	Sinks   []int64 `json:"sinks"`
}

var (
	apiURL   = "http://localhost:8080"
	failures int
)

func main() {
	if v := os.Getenv("API_URL"); v != "" {
		apiURL = v
	}
	waitReady()

	fmt.Println("== exact-cost cases ==")
	checkExact("parallel edges are billed individually", graphRequest{
		N: 3,
		Edges: []edge{
			{From: 0, To: 1, Cost: 3},
			{From: 0, To: 1, Cost: 4},
			{From: 1, To: 2, Cost: 10},
		},
		Sources: []int64{0}, Sinks: []int64{2},
	}, 7)

	checkExact("self loops do not affect the result", graphRequest{
		N: 3,
		Edges: []edge{
			{From: 0, To: 1, Cost: 3},
			{From: 0, To: 1, Cost: 4},
			{From: 1, To: 2, Cost: 10},
			{From: 0, To: 0, Cost: 1},
			{From: 1, To: 1, Cost: 2},
			{From: 2, To: 2, Cost: 1000000000},
		},
		Sources: []int64{0}, Sinks: []int64{2},
	}, 7)

	checkExact("multi source multi sink", graphRequest{
		N: 6,
		Edges: []edge{
			{From: 0, To: 2, Cost: 4},
			{From: 1, To: 2, Cost: 4},
			{From: 2, To: 3, Cost: 3},
			{From: 3, To: 4, Cost: 4},
			{From: 3, To: 5, Cost: 4},
		},
		Sources: []int64{0, 1}, Sinks: []int64{4, 5},
	}, 3)

	checkExact("no path costs zero", graphRequest{
		N:       4,
		Edges:   []edge{{From: 0, To: 1, Cost: 9}, {From: 2, To: 3, Cost: 9}},
		Sources: []int64{0}, Sinks: []int64{3},
	}, 0)

	checkExact("64 bit answer", graphRequest{
		N: 2,
		Edges: []edge{
			{From: 0, To: 1, Cost: 1_000_000_000},
			{From: 0, To: 1, Cost: 1_000_000_000},
			{From: 0, To: 1, Cost: 1_000_000_000},
		},
		Sources: []int64{0}, Sinks: []int64{1},
	}, 3_000_000_000)

	checkExact("cheapest side of the network is chosen", graphRequest{
		N: 4,
		Edges: []edge{
			{From: 0, To: 1, Cost: 8},
			{From: 0, To: 2, Cost: 3},
			{From: 1, To: 3, Cost: 4},
			{From: 2, To: 3, Cost: 10},
		},
		Sources: []int64{0}, Sinks: []int64{3},
	}, 7)

	fmt.Println("== invalid graphs must be rejected with a stable 422 envelope ==")
	checkInvalidGraphs()

	fmt.Println("== 20000 nodes / 100000 edges within a 10s request timeout ==")
	checkLarge()

	fmt.Println()
	if failures > 0 {
		fmt.Printf("VERIFY FAILED: %d check(s) failed\n", failures)
		os.Exit(1)
	}
	fmt.Println("VERIFY PASSED: all acceptance checks succeeded")
}

// waitReady polls /healthz until the API answers or the deadline passes.
func waitReady() {
	deadline := time.Now().Add(60 * time.Second)
	for {
		resp, err := http.Get(apiURL + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				fmt.Printf("api ready at %s\n", apiURL)
				return
			}
		}
		if time.Now().After(deadline) {
			fmt.Println("VERIFY FAILED: api did not become ready within 60s")
			os.Exit(1)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// post sends body to the endpoint with the given timeout and returns the
// status code plus the decoded JSON document.
func post(timeout time.Duration, body []byte) (int, map[string]json.RawMessage, time.Duration, error) {
	client := &http.Client{Timeout: timeout}
	start := time.Now()
	resp, err := http.NewRequest(http.MethodPost, apiURL+"/minimum-shutdown-cost", bytes.NewReader(body))
	if err != nil {
		return 0, nil, 0, err
	}
	resp.Header.Set("Content-Type", "application/json")
	res, err := client.Do(resp)
	elapsed := time.Since(start)
	if err != nil {
		return 0, nil, elapsed, err
	}
	defer res.Body.Close()
	var doc map[string]json.RawMessage
	if err := json.NewDecoder(res.Body).Decode(&doc); err != nil {
		return res.StatusCode, nil, elapsed, fmt.Errorf("response is not JSON: %w", err)
	}
	return res.StatusCode, doc, elapsed, nil
}

func checkExact(name string, req graphRequest, want int64) {
	body, err := json.Marshal(req)
	if err != nil {
		record(name, fmt.Errorf("marshal: %w", err))
		return
	}
	status, doc, elapsed, err := post(10*time.Second, body)
	if err != nil {
		record(name, err)
		return
	}
	if status != http.StatusOK {
		record(name, fmt.Errorf("status = %d, want 200 (body %v)", status, doc))
		return
	}
	raw, ok := doc["minimum_shutdown_cost"]
	if !ok {
		record(name, fmt.Errorf("response missing minimum_shutdown_cost: %v", doc))
		return
	}
	var got int64
	if err := json.Unmarshal(raw, &got); err != nil {
		record(name, fmt.Errorf("minimum_shutdown_cost is not an int64: %s", raw))
		return
	}
	if got != want {
		record(name, fmt.Errorf("minimum_shutdown_cost = %d, want %d", got, want))
		return
	}
	pass(fmt.Sprintf("%s (cost=%d, %s)", name, got, elapsed.Round(time.Millisecond)))
}

func checkInvalidGraphs() {
	tooManyEdges := graphRequest{N: 2, Sources: []int64{0}, Sinks: []int64{1}}
	for i := 0; i <= 100000; i++ {
		tooManyEdges.Edges = append(tooManyEdges.Edges, edge{From: 0, To: 1, Cost: 1})
	}
	tooMany, _ := json.Marshal(tooManyEdges)

	cases := []struct {
		name string
		body []byte
	}{
		{"n below minimum", []byte(`{"n":1,"edges":[],"sources":[0],"sinks":[0]}`)},
		{"n above maximum", []byte(`{"n":20001,"edges":[],"sources":[0],"sinks":[1]}`)},
		{"edge endpoint out of range", []byte(`{"n":2,"edges":[{"from":0,"to":2,"cost":1}],"sources":[0],"sinks":[1]}`)},
		{"negative edge endpoint", []byte(`{"n":2,"edges":[{"from":-1,"to":1,"cost":1}],"sources":[0],"sinks":[1]}`)},
		{"zero cost", []byte(`{"n":2,"edges":[{"from":0,"to":1,"cost":0}],"sources":[0],"sinks":[1]}`)},
		{"cost above maximum", []byte(`{"n":2,"edges":[{"from":0,"to":1,"cost":1000000001}],"sources":[0],"sinks":[1]}`)},
		{"non-integer cost", []byte(`{"n":2,"edges":[{"from":0,"to":1,"cost":1.5}],"sources":[0],"sinks":[1]}`)},
		{"empty sources", []byte(`{"n":2,"edges":[],"sources":[],"sinks":[1]}`)},
		{"empty sinks", []byte(`{"n":2,"edges":[],"sources":[0],"sinks":[]}`)},
		{"source/sink overlap", []byte(`{"n":3,"edges":[],"sources":[0,1],"sinks":[1,2]}`)},
		{"more than 100000 edges", tooMany},
		{"malformed json", []byte(`{"n":2,"edges":[`)},
	}

	var keyShape string
	for _, tc := range cases {
		status, doc, _, err := post(10*time.Second, tc.body)
		switch {
		case err != nil:
			record("invalid: "+tc.name, err)
		case status != http.StatusUnprocessableEntity:
			record("invalid: "+tc.name, fmt.Errorf("status = %d, want 422", status))
		default:
			shape := envelopeShape(doc)
			if shape == "" {
				record("invalid: "+tc.name, fmt.Errorf("missing error envelope in %v", doc))
			} else if keyShape == "" {
				keyShape = shape
				pass(fmt.Sprintf("invalid: %s -> 422 %s", tc.name, shape))
			} else if shape != keyShape {
				record("invalid: "+tc.name, fmt.Errorf("error shape %s differs from %s", shape, keyShape))
			} else {
				pass(fmt.Sprintf("invalid: %s -> 422 %s", tc.name, shape))
			}
		}
	}
}

// envelopeShape returns a canonical description of the error document, or ""
// when it does not match {"error":{"code":string,"message":string}}.
func envelopeShape(doc map[string]json.RawMessage) string {
	if len(doc) != 1 {
		return ""
	}
	raw, ok := doc["error"]
	if !ok {
		return ""
	}
	var env struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.Code == "" || env.Message == "" {
		return ""
	}
	return `{"error":{"code","message"}}`
}

// largeGraph builds the 20000-node / 100000-edge acceptance graph and the
// exact expected cost, which is known by construction:
//
//   - Nodes are split into halves A (sources' side) and B (sinks' side).
//   - A directed cycle of 1e9-cost pipes runs through every node of each
//     half, so any cut that does not respect the A/B split severs at least
//     one 1e9 pipe.
//   - The only cheap A->B pipes are 200 crossing edges whose costs sum to
//     the expected answer (< 2e8 << 1e9).
//   - Everything else (self-loops, parallel intra-half pipes, reverse B->A
//     pipes) cannot belong to a cheaper cut.
func largeGraph() (graphRequest, int64) {
	const (
		n        = 20000
		half     = n / 2
		crossing = 200
		heavy    = 1_000_000_000
	)
	rng := rand.New(rand.NewSource(20260916))
	edges := make([]edge, 0, 100000)

	// Anchor cycles: every node sits on a 1e9 cycle inside its own half.
	for i := 0; i < half; i++ {
		edges = append(edges, edge{From: int64(i), To: int64((i + 1) % half), Cost: heavy})
		edges = append(edges, edge{From: int64(half + i), To: int64(half + (i+1)%half), Cost: heavy})
	}
	// Crossing pipes A -> B: these and only these define the answer.
	var want int64
	for k := 0; k < crossing; k++ {
		c := 1 + rng.Int63n(1_000_000)
		edges = append(edges, edge{From: int64(rng.Intn(half)), To: int64(half + rng.Intn(half)), Cost: c})
		want += c
	}
	// Reverse pipes B -> A never cross a source->sink cut.
	for k := 0; k < crossing; k++ {
		edges = append(edges, edge{From: int64(half + rng.Intn(half)), To: int64(rng.Intn(half)), Cost: 1 + rng.Int63n(1_000_000)})
	}
	// Fill up to exactly 100000 edges with self-loops and heavy intra-half
	// pipes (parallel edges occur naturally).
	for len(edges) < 100000 {
		switch rng.Intn(3) {
		case 0:
			v := int64(rng.Intn(n))
			edges = append(edges, edge{From: v, To: v, Cost: 1 + rng.Int63n(heavy)})
		case 1:
			edges = append(edges, edge{From: int64(rng.Intn(half)), To: int64(rng.Intn(half)), Cost: heavy})
		default:
			edges = append(edges, edge{From: int64(half + rng.Intn(half)), To: int64(half + rng.Intn(half)), Cost: heavy})
		}
	}

	sources := make([]int64, 0, 50)
	for i := 0; i < 50; i++ {
		sources = append(sources, int64(i))
	}
	sinks := make([]int64, 0, 50)
	for i := 0; i < 50; i++ {
		sinks = append(sinks, int64(n-50+i))
	}
	return graphRequest{N: n, Edges: edges, Sources: sources, Sinks: sinks}, want
}

func checkLarge() {
	req, want := largeGraph()
	body, err := json.Marshal(req)
	if err != nil {
		record("large graph", fmt.Errorf("marshal: %w", err))
		return
	}
	status, doc, elapsed, err := post(10*time.Second, body)
	if err != nil {
		record("large graph", fmt.Errorf("request failed (timeout 10s?): %w", err))
		return
	}
	if status != http.StatusOK {
		record("large graph", fmt.Errorf("status = %d, want 200 (body %v)", status, doc))
		return
	}
	var got int64
	if err := json.Unmarshal(doc["minimum_shutdown_cost"], &got); err != nil {
		record("large graph", fmt.Errorf("bad response: %v", doc))
		return
	}
	if got != want {
		record("large graph", fmt.Errorf("minimum_shutdown_cost = %d, want %d", got, want))
		return
	}
	if elapsed > 10*time.Second {
		record("large graph", fmt.Errorf("took %v, over the 10s request timeout", elapsed))
		return
	}
	pass(fmt.Sprintf("large graph n=%d edges=%d cost=%d in %v",
		req.N, len(req.Edges), got, elapsed.Round(time.Millisecond)))
}

func pass(msg string) {
	fmt.Printf("  PASS  %s\n", msg)
}

func record(name string, err error) {
	failures++
	fmt.Printf("  FAIL  %s: %v\n", name, err)
}
