// Command verify is the one-shot acceptance client. It assumes the Go unit
// tests have already been run by the caller (the Docker Compose "verify"
// service runs `go test ./...` first), then exercises a live API server:
// exact shutdown costs for parallel edges, self-loops and multi-source /
// multi-sink graphs, a stable 422 error envelope for invalid graphs, and a
// 20000-node / 100000-edge graph whose exact cost must be returned within a
// 10 second request timeout. It also accepts the contamination-survey API:
// target derivation, the PENDING/IN_PROGRESS/COMPLETED lifecycle, the
// conclusion, the 404/409/422 error envelope, and concurrent sampling where
// records must be complete, duplicate-free and concluded exactly once.
//
// The API base URL is read from API_URL (default http://localhost:8080).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sync"
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

	fmt.Println("== investigations: targets, lifecycle, conclusion ==")
	checkInvestigationTargets()
	checkInvestigationLifecycle()
	checkInvestigationCleanConclusion()
	checkInvestigationNoTargets()

	fmt.Println("== investigations: stable error envelope (422 / 404 / 409) ==")
	checkInvestigationInvalidCreates()
	checkInvestigationSampleErrors()

	fmt.Println("== investigations: concurrent sampling ==")
	checkInvestigationConcurrentTargets()
	checkInvestigationConcurrentLastTarget()

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

// doPost sends body to path with the given timeout and returns the status
// code plus the raw response body.
func doPost(path string, timeout time.Duration, body []byte) (int, []byte, time.Duration, error) {
	client := &http.Client{Timeout: timeout}
	start := time.Now()
	req, err := http.NewRequest(http.MethodPost, apiURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		return 0, nil, elapsed, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return res.StatusCode, nil, elapsed, fmt.Errorf("read body: %w", err)
	}
	return res.StatusCode, raw, elapsed, nil
}

// post sends body to the minimum-shutdown-cost endpoint with the given
// timeout and returns the status code plus the decoded JSON document.
func post(timeout time.Duration, body []byte) (int, map[string]json.RawMessage, time.Duration, error) {
	status, raw, elapsed, err := doPost("/minimum-shutdown-cost", timeout, body)
	if err != nil {
		return 0, nil, elapsed, err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return status, nil, elapsed, fmt.Errorf("response is not JSON: %w", err)
	}
	return status, doc, elapsed, nil
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

// ---------------------------------------------------------------------------
// Contamination survey (investigations) acceptance
// ---------------------------------------------------------------------------

type invSample struct {
	SampleID string `json:"sample_id"`
	Node     int64  `json:"node"`
	Result   string `json:"result"`
}

// invView is the full investigation document returned by the API.
type invView struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Snapshot   struct {
		N     int64 `json:"n"`
		Edges []struct {
			From int64 `json:"from"`
			To   int64 `json:"to"`
		} `json:"edges"`
		Inlets []int64 `json:"inlets"`
	} `json:"snapshot"`
	Targets   []int64     `json:"targets"`
	Samples   []invSample `json:"samples"`
	CreatedAt string      `json:"created_at"`
}

// createInvestigation posts body to /investigations and expects 201 plus a
// full investigation view.
func createInvestigation(body []byte) (invView, error) {
	status, raw, _, err := doPost("/investigations", 10*time.Second, body)
	if err != nil {
		return invView{}, err
	}
	if status != http.StatusCreated {
		return invView{}, fmt.Errorf("create status = %d, want 201 (body %s)", status, raw)
	}
	var view invView
	if err := json.Unmarshal(raw, &view); err != nil {
		return invView{}, fmt.Errorf("create response is not an investigation view: %w", err)
	}
	if view.ID == "" || view.Status == "" || view.CreatedAt == "" {
		return invView{}, fmt.Errorf("create view misses id/status/created_at: %s", raw)
	}
	if view.Targets == nil || view.Samples == nil {
		return invView{}, fmt.Errorf("targets and samples must be arrays, got %s", raw)
	}
	return view, nil
}

// postSample posts one sample document and returns the status, the decoded
// view when the status is 200, and the raw body otherwise.
func postSample(id string, body []byte) (int, invView, []byte, error) {
	status, raw, _, err := doPost("/investigations/"+id+"/samples", 10*time.Second, body)
	if err != nil {
		return 0, invView{}, nil, err
	}
	if status != http.StatusOK {
		return status, invView{}, raw, nil
	}
	var view invView
	if err := json.Unmarshal(raw, &view); err != nil {
		return status, invView{}, raw, fmt.Errorf("sample response is not an investigation view: %w", err)
	}
	return status, view, raw, nil
}

func sampleBody(sampleID string, node int64, result string) []byte {
	body, _ := json.Marshal(invSample{SampleID: sampleID, Node: node, Result: result})
	return body
}

func sameNodes(got []int64, want ...int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// checkInvestigationTargets verifies the reachable-target derivation against
// a graph full of traps: parallel edges, self-loops, a cycle back into an
// inlet, an inlet reachable from another inlet, and a disconnected component.
func checkInvestigationTargets() {
	name := "targets exclude inlets and ignore cycles/self-loops/parallel edges"
	view, err := createInvestigation([]byte(`{
		"n": 6,
		"edges": [
			{"from":0,"to":2},{"from":0,"to":2},
			{"from":2,"to":3},{"from":3,"to":0},
			{"from":1,"to":3},{"from":3,"to":3},
			{"from":0,"to":0},{"from":5,"to":4}
		],
		"inlets": [0,1]
	}`))
	if err != nil {
		record(name, err)
		return
	}
	switch {
	case !sameNodes(view.Targets, 2, 3):
		record(name, fmt.Errorf("targets = %v, want [2 3]", view.Targets))
	case view.Status != "PENDING":
		record(name, fmt.Errorf("status = %s, want PENDING", view.Status))
	case view.Conclusion != "clean":
		record(name, fmt.Errorf("conclusion = %s, want clean", view.Conclusion))
	case len(view.Samples) != 0:
		record(name, fmt.Errorf("samples = %v, want none", view.Samples))
	case view.Snapshot.N != 6 || len(view.Snapshot.Edges) != 8 || !sameNodes(view.Snapshot.Inlets, 0, 1):
		record(name, fmt.Errorf("snapshot mismatch: %+v", view.Snapshot))
	default:
		pass(fmt.Sprintf("%s (targets=%v)", name, view.Targets))
	}
}

// checkInvestigationLifecycle walks one investigation through
// PENDING -> IN_PROGRESS -> COMPLETED, checks the contaminated conclusion and
// that the terminal state rejects further writes.
func checkInvestigationLifecycle() {
	view, err := createInvestigation([]byte(
		`{"n":4,"edges":[{"from":0,"to":1},{"from":1,"to":2}],"inlets":[0]}`))
	if err != nil {
		record("lifecycle: create", err)
		return
	}
	id := view.ID

	status, view, raw, err := postSample(id, sampleBody("life-1", 1, "clean"))
	switch {
	case err != nil:
		record("lifecycle: first sample", err)
		return
	case status != http.StatusOK:
		record("lifecycle: first sample", fmt.Errorf("status = %d, want 200 (body %s)", status, raw))
		return
	case view.Status != "IN_PROGRESS" || view.Conclusion != "clean" || len(view.Samples) != 1:
		record("lifecycle: first sample", fmt.Errorf("view = %+v, want IN_PROGRESS/clean/1 sample", view))
		return
	}

	status, view, raw, err = postSample(id, sampleBody("life-2", 2, "contaminated"))
	switch {
	case err != nil:
		record("lifecycle: completing sample", err)
		return
	case status != http.StatusOK:
		record("lifecycle: completing sample", fmt.Errorf("status = %d, want 200 (body %s)", status, raw))
		return
	case view.Status != "COMPLETED":
		record("lifecycle: completing sample", fmt.Errorf("status = %s, want COMPLETED", view.Status))
		return
	case view.Conclusion != "contaminated":
		record("lifecycle: completing sample", fmt.Errorf("conclusion = %s, want contaminated", view.Conclusion))
		return
	case len(view.Samples) != 2:
		record("lifecycle: completing sample", fmt.Errorf("samples = %d, want 2", len(view.Samples)))
		return
	}

	status, raw, _, err = doPost("/investigations/"+id+"/samples", 10*time.Second, sampleBody("life-3", 1, "clean"))
	switch {
	case err != nil:
		record("lifecycle: terminal write", err)
		return
	case status != http.StatusConflict:
		record("lifecycle: terminal write", fmt.Errorf("status = %d, want 409 (body %s)", status, raw))
		return
	}
	pass("lifecycle PENDING -> IN_PROGRESS -> COMPLETED, conclusion contaminated, terminal write -> 409")
}

// checkInvestigationCleanConclusion: all-clean samples conclude clean.
func checkInvestigationCleanConclusion() {
	name := "all-clean samples conclude clean"
	view, err := createInvestigation([]byte(
		`{"n":3,"edges":[{"from":0,"to":1},{"from":1,"to":2}],"inlets":[0]}`))
	if err != nil {
		record(name, err)
		return
	}
	if _, _, _, err := postSample(view.ID, sampleBody("clean-1", 1, "clean")); err != nil {
		record(name, err)
		return
	}
	_, view, _, err = postSample(view.ID, sampleBody("clean-2", 2, "clean"))
	switch {
	case err != nil:
		record(name, err)
	case view.Status != "COMPLETED" || view.Conclusion != "clean":
		record(name, fmt.Errorf("got (%s, %s), want (COMPLETED, clean)", view.Status, view.Conclusion))
	default:
		pass(name)
	}
}

// checkInvestigationNoTargets: an investigation without reachable targets is
// COMPLETED at creation and stays terminal.
func checkInvestigationNoTargets() {
	name := "no targets completes at creation"
	view, err := createInvestigation([]byte(
		`{"n":2,"edges":[{"from":0,"to":0}],"inlets":[0]}`))
	if err != nil {
		record(name, err)
		return
	}
	switch {
	case view.Status != "COMPLETED":
		record(name, fmt.Errorf("status = %s, want COMPLETED", view.Status))
		return
	case len(view.Targets) != 0:
		record(name, fmt.Errorf("targets = %v, want none", view.Targets))
		return
	case view.Conclusion != "clean":
		record(name, fmt.Errorf("conclusion = %s, want clean", view.Conclusion))
		return
	}
	status, raw, _, err := doPost("/investigations/"+view.ID+"/samples", 10*time.Second,
		sampleBody("none-1", 0, "clean"))
	switch {
	case err != nil:
		record(name, err)
	case status != http.StatusConflict:
		record(name, fmt.Errorf("terminal write status = %d, want 409 (body %s)", status, raw))
	default:
		pass(name + "; terminal write -> 409")
	}
}

// checkInvestigationInvalidCreates: invalid snapshots are rejected with 422
// and the stable error envelope.
func checkInvestigationInvalidCreates() {
	cases := []struct {
		name string
		body []byte
	}{
		{"n zero", []byte(`{"n":0,"edges":[],"inlets":[0]}`)},
		{"n above maximum", []byte(`{"n":20001,"edges":[],"inlets":[0]}`)},
		{"edge endpoint out of range", []byte(`{"n":2,"edges":[{"from":0,"to":2}],"inlets":[0]}`)},
		{"negative edge endpoint", []byte(`{"n":2,"edges":[{"from":-1,"to":1}],"inlets":[0]}`)},
		{"empty inlets", []byte(`{"n":2,"edges":[],"inlets":[]}`)},
		{"inlet out of range", []byte(`{"n":2,"edges":[],"inlets":[2]}`)},
		{"unknown field", []byte(`{"n":2,"edges":[],"inlets":[0],"debug":true}`)},
		{"malformed json", []byte(`{"n":2,"edges":[`)},
	}
	for _, tc := range cases {
		status, raw, _, err := doPost("/investigations", 10*time.Second, tc.body)
		switch {
		case err != nil:
			record("invalid create: "+tc.name, err)
		case status != http.StatusUnprocessableEntity:
			record("invalid create: "+tc.name, fmt.Errorf("status = %d, want 422 (body %s)", status, raw))
		default:
			var doc map[string]json.RawMessage
			if err := json.Unmarshal(raw, &doc); err != nil || envelopeShape(doc) == "" {
				record("invalid create: "+tc.name, fmt.Errorf("missing error envelope in %s", raw))
			} else {
				pass(fmt.Sprintf("invalid create: %s -> 422 %s", tc.name, envelopeShape(doc)))
			}
		}
	}
}

// checkInvestigationSampleErrors: unknown investigations answer 404, invalid
// sample documents 422, and conflicts 409, all with the stable envelope; the
// rejected writes must leave no trace.
func checkInvestigationSampleErrors() {
	view, err := createInvestigation([]byte(
		`{"n":5,"edges":[{"from":0,"to":1},{"from":0,"to":2}],"inlets":[0]}`))
	if err != nil {
		record("sample errors: create", err)
		return
	}
	id := view.ID

	expectEnvelope := func(name string, wantStatus int, body []byte) bool {
		status, raw, _, err := doPost("/investigations/"+id+"/samples", 10*time.Second, body)
		if err != nil {
			record(name, err)
			return false
		}
		if status != wantStatus {
			record(name, fmt.Errorf("status = %d, want %d (body %s)", status, wantStatus, raw))
			return false
		}
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(raw, &doc); err != nil || envelopeShape(doc) == "" {
			record(name, fmt.Errorf("missing error envelope in %s", raw))
			return false
		}
		return true
	}

	// Unknown investigation -> 404.
	status, raw, _, err := doPost("/investigations/999999/samples", 10*time.Second, sampleBody("ghost", 1, "clean"))
	switch {
	case err != nil:
		record("sample errors: unknown investigation", err)
	case status != http.StatusNotFound:
		record("sample errors: unknown investigation", fmt.Errorf("status = %d, want 404 (body %s)", status, raw))
	default:
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(raw, &doc); err != nil || envelopeShape(doc) == "" {
			record("sample errors: unknown investigation", fmt.Errorf("missing error envelope in %s", raw))
		} else {
			pass("sample errors: unknown investigation -> 404 " + envelopeShape(doc))
		}
	}

	// Invalid sample documents -> 422.
	invalid := []struct {
		name string
		body []byte
	}{
		{"empty sample_id", sampleBody("", 1, "clean")},
		{"missing node", []byte(`{"sample_id":"x","result":"clean"}`)},
		{"negative node", sampleBody("x", -1, "clean")},
		{"unknown result", sampleBody("x", 1, "murky")},
		{"malformed json", []byte(`{"sample_id":"x",`)},
	}
	for _, tc := range invalid {
		if expectEnvelope("sample errors: "+tc.name, http.StatusUnprocessableEntity, tc.body) {
			pass(fmt.Sprintf("sample errors: %s -> 422", tc.name))
		}
	}

	// Conflicts -> 409.
	if _, _, _, err := postSample(id, sampleBody("taken", 1, "clean")); err != nil {
		record("sample errors: seed sample", err)
		return
	}
	conflicts := []struct {
		name string
		body []byte
	}{
		{"duplicate sample_id", sampleBody("taken", 2, "clean")},
		{"node already sampled", sampleBody("fresh", 1, "clean")},
		{"node not a target", sampleBody("other", 3, "clean")},
		{"node out of range", sampleBody("other2", 99, "clean")},
		{"inlet is not a target", sampleBody("other3", 0, "clean")},
	}
	for _, tc := range conflicts {
		if expectEnvelope("sample errors: "+tc.name, http.StatusConflict, tc.body) {
			pass(fmt.Sprintf("sample errors: %s -> 409", tc.name))
		}
	}

	// None of the rejected writes may have left a trace: the investigation
	// completes with exactly the two valid samples.
	_, view, _, err = postSample(id, sampleBody("final", 2, "clean"))
	switch {
	case err != nil:
		record("sample errors: no trace", err)
	case view.Status != "COMPLETED" || len(view.Samples) != 2:
		record("sample errors: no trace", fmt.Errorf("view = %+v, want COMPLETED with exactly 2 samples", view))
	default:
		pass("sample errors: rejected writes left no trace")
	}
}

// checkInvestigationConcurrentTargets fires one sample per target plus
// duplicate impostors at one investigation concurrently: exactly one sample
// per target may be recorded and the investigation must conclude exactly
// once.
func checkInvestigationConcurrentTargets() {
	name := "concurrent samples on distinct targets"
	const targets = 40
	edges := make([]map[string]int64, 0, targets)
	for i := 0; i < targets; i++ {
		edges = append(edges, map[string]int64{"from": int64(i), "to": int64(i + 1)})
	}
	createBody, _ := json.Marshal(map[string]any{"n": targets + 1, "edges": edges, "inlets": []int64{0}})
	view, err := createInvestigation(createBody)
	if err != nil {
		record(name, err)
		return
	}
	if len(view.Targets) != targets {
		record(name, fmt.Errorf("targets = %d, want %d", len(view.Targets), targets))
		return
	}

	type outcome struct {
		status int
		view   invView
	}
	const extras = 6
	results := make(chan outcome, targets+extras)
	var wg sync.WaitGroup
	fire := func(body []byte) {
		defer wg.Done()
		status, raw, _, err := doPost("/investigations/"+view.ID+"/samples", 10*time.Second, body)
		if err != nil {
			record(name, err)
			results <- outcome{status: -1}
			return
		}
		var v invView
		if status == http.StatusOK {
			if err := json.Unmarshal(raw, &v); err != nil {
				record(name, fmt.Errorf("bad success view: %w", err))
			}
		}
		results <- outcome{status: status, view: v}
	}
	for node := int64(1); node <= targets; node++ {
		result := "clean"
		if node == 20 {
			result = "contaminated"
		}
		sample := sampleBody(fmt.Sprintf("c-%d", node), node, result)
		wg.Add(1)
		go fire(sample)
	}
	// Exact duplicates of one sample and node-duplicates of another: every
	// one of them must lose against the original.
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go fire(sampleBody("c-5", 5, "clean"))
		wg.Add(1)
		go fire(sampleBody(fmt.Sprintf("impostor-%d", i), 7, "clean"))
	}
	wg.Wait()
	close(results)

	var ok, conflict, other, completedViews int
	var final invView
	for out := range results {
		switch out.status {
		case http.StatusOK:
			ok++
			if out.view.Status == "COMPLETED" {
				completedViews++
				final = out.view
			}
		case http.StatusConflict:
			conflict++
		default:
			other++
		}
	}
	switch {
	case other != 0:
		record(name, fmt.Errorf("%d unexpected statuses", other))
	case ok != targets:
		record(name, fmt.Errorf("successful samples = %d, want %d", ok, targets))
	case conflict != extras:
		record(name, fmt.Errorf("conflicts = %d, want %d", conflict, extras))
	case completedViews != 1:
		record(name, fmt.Errorf("COMPLETED views = %d, want exactly 1", completedViews))
	case len(final.Samples) != targets:
		record(name, fmt.Errorf("final samples = %d, want %d", len(final.Samples), targets))
	default:
		seenIDs := make(map[string]bool)
		seenNodes := make(map[int64]bool)
		for _, s := range final.Samples {
			seenIDs[s.SampleID] = true
			seenNodes[s.Node] = true
		}
		missing := 0
		for node := int64(1); node <= targets; node++ {
			if !seenNodes[node] {
				missing++
			}
		}
		switch {
		case len(seenIDs) != targets || len(seenNodes) != targets || missing != 0:
			record(name, fmt.Errorf("final samples not duplicate-free/complete: ids=%d nodes=%d missing=%d",
				len(seenIDs), len(seenNodes), missing))
		case final.Conclusion != "contaminated":
			record(name, fmt.Errorf("conclusion = %s, want contaminated", final.Conclusion))
		default:
			pass(fmt.Sprintf("%s (%d ok, %d conflicts, concluded once, %d samples)",
				name, ok, conflict, len(final.Samples)))
		}
	}
}

// checkInvestigationConcurrentLastTarget: many clients race to sample the
// single remaining target; exactly one may succeed and conclude the
// investigation.
func checkInvestigationConcurrentLastTarget() {
	name := "concurrent race for the last target"
	view, err := createInvestigation([]byte(
		`{"n":3,"edges":[{"from":0,"to":1},{"from":0,"to":2}],"inlets":[0]}`))
	if err != nil {
		record(name, err)
		return
	}
	if _, _, _, err := postSample(view.ID, sampleBody("first", 1, "clean")); err != nil {
		record(name, err)
		return
	}

	const racers = 8
	results := make(chan int, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			status, _, _, err := postSample(view.ID, sampleBody(fmt.Sprintf("racer-%d", i), 2, "clean"))
			if err != nil {
				record(name, err)
				results <- -1
				return
			}
			results <- status
		}(i)
	}
	wg.Wait()
	close(results)

	var ok, conflict, other int
	for status := range results {
		switch status {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			conflict++
		default:
			other++
		}
	}
	switch {
	case other != 0:
		record(name, fmt.Errorf("%d unexpected statuses", other))
	case ok != 1 || conflict != racers-1:
		record(name, fmt.Errorf("ok = %d, conflicts = %d, want 1 and %d", ok, conflict, racers-1))
	default:
		pass(fmt.Sprintf("%s (1 winner, %d conflicts)", name, conflict))
	}
}
