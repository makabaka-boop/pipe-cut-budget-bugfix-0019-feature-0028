package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"

	"raincut/internal/investigation"
)

// resetInvestigationRepo swaps in a fresh repository so the package-global
// store starts empty for these tests.
func resetInvestigationRepo() {
	investigationRepo = investigation.NewRepository()
}

// investigationView is the typed success response used by the tests.
type investigationView struct {
	ID    string `json:"id"`
	N     int    `json:"n"`
	Edges []struct {
		From int `json:"from"`
		To   int `json:"to"`
	} `json:"edges"`
	Sources []int   `json:"sources"`
	Targets []int   `json:"targets"`
	Status  string  `json:"status"`
	Result  *string `json:"result"`
	Samples []struct {
		SampleID string `json:"sample_id"`
		Node     int    `json:"node"`
		Result   string `json:"result"`
	} `json:"samples"`
}

func doRaw(t *testing.T, method, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	New().ServeHTTP(rec, req)

	var parsed map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("response is not valid JSON: %v (%q)", err, rec.Body.String())
	}
	return rec.Code, parsed
}

func createInvestigation(t *testing.T, body string) investigationView {
	t.Helper()
	status, raw := doRaw(t, http.MethodPost, "/investigations", body)
	if status != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (body %v)", status, raw)
	}
	view := decodeView(t, raw)
	return view
}

func decodeView(t *testing.T, raw map[string]any) investigationView {
	t.Helper()
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var view investigationView
	if err := json.Unmarshal(b, &view); err != nil {
		t.Fatalf("decode view: %v (%v)", err, raw)
	}
	return view
}

func addSample(t *testing.T, id, sampleID string, node int, result string) (int, investigationView) {
	t.Helper()
	body := fmt.Sprintf(`{"sample_id":%q,"node":%d,"result":%q}`, sampleID, node, result)
	status, raw := doRaw(t, http.MethodPost, "/investigations/"+id+"/samples", body)
	return status, decodeView(t, raw)
}

func TestCreateInvestigationTargetSet(t *testing.T) {
	resetInvestigationRepo()

	// Parallel pipes (0->1 twice), a self-loop (1->1), a cycle (2->1) and
	// two inlets: each reachable node must appear exactly once and no inlet
	// may appear.
	body := `{"n":5,"edges":[
		{"from":0,"to":1},{"from":0,"to":1},
		{"from":1,"to":1},{"from":1,"to":2},{"from":2,"to":1},
		{"from":2,"to":3},{"from":3,"to":4}],
		"sources":[0]}`
	view := createInvestigation(t, body)

	want := []int{1, 2, 3, 4}
	if fmt.Sprint(view.Targets) != fmt.Sprint(want) {
		t.Fatalf("targets = %v, want %v", view.Targets, want)
	}
	if view.Status != "PENDING" || view.Result != nil || len(view.Samples) != 0 {
		t.Fatalf("view = %+v, want PENDING with no result/samples", view)
	}
	if len(view.ID) == 0 || view.ID[:4] != "inv_" {
		t.Fatalf("id = %q, want an inv_ prefixed identifier", view.ID)
	}
	if view.N != 5 {
		t.Fatalf("n = %d, want 5", view.N)
	}
}

func TestCreateWithoutTargetsIsImmediatelyCompleted(t *testing.T) {
	resetInvestigationRepo()
	view := createInvestigation(t, `{"n":2,"edges":[],"sources":[0]}`)
	if view.Status != "COMPLETED" {
		t.Fatalf("status = %s, want COMPLETED", view.Status)
	}
	if view.Result == nil || *view.Result != "clean" {
		t.Fatalf("result = %v, want clean", view.Result)
	}
}

func TestSamplingLifecycleAndConclusion(t *testing.T) {
	resetInvestigationRepo()
	// Targets: 1,2,3.
	view := createInvestigation(t, `{"n":4,"edges":[
		{"from":0,"to":1},{"from":1,"to":2},{"from":2,"to":3}],"sources":[0]}`)

	status, v := addSample(t, view.ID, "s-1", 1, "clean")
	if status != http.StatusCreated || v.Status != "IN_PROGRESS" || v.Result != nil || len(v.Samples) != 1 {
		t.Fatalf("first sample: status=%d view=%+v", status, v)
	}

	// A contaminated verdict mid-flight keeps the investigation open.
	status, v = addSample(t, view.ID, "s-2", 3, "contaminated")
	if status != http.StatusCreated || v.Status != "IN_PROGRESS" || v.Result != nil {
		t.Fatalf("second sample: status=%d view=%+v", status, v)
	}

	status, v = addSample(t, view.ID, "s-3", 2, "clean")
	if status != http.StatusCreated || v.Status != "COMPLETED" || v.Result == nil || *v.Result != "contaminated" {
		t.Fatalf("completing sample: status=%d view=%+v", status, v)
	}
	if len(v.Samples) != 3 {
		t.Fatalf("samples = %d, want 3", len(v.Samples))
	}
	// Sorted by node.
	for i, node := range []int{1, 2, 3} {
		if v.Samples[i].Node != node {
			t.Fatalf("samples not sorted by node: %+v", v.Samples)
		}
	}

	// Terminal state never regresses: any further write is a 409.
	status, raw := doRaw(t, http.MethodPost, "/investigations/"+view.ID+"/samples",
		`{"sample_id":"s-4","node":1,"result":"clean"}`)
	if status != http.StatusConflict {
		t.Fatalf("terminal write status = %d, want 409 (body %v)", status, raw)
	}
	assertErrorEnvelope(t, raw)
}

func TestCleanConclusion(t *testing.T) {
	resetInvestigationRepo()
	view := createInvestigation(t, `{"n":2,"edges":[{"from":0,"to":1}],"sources":[0]}`)
	status, v := addSample(t, view.ID, "ok", 1, "clean")
	if status != http.StatusCreated || v.Status != "COMPLETED" || v.Result == nil || *v.Result != "clean" {
		t.Fatalf("view = %+v, want COMPLETED/clean", v)
	}
}

func TestSampleErrors(t *testing.T) {
	resetInvestigationRepo()
	view := createInvestigation(t, `{"n":3,"edges":[{"from":0,"to":1},{"from":1,"to":2}],"sources":[0]}`)

	// Unknown investigation -> 404 even with a well-formed body.
	status, raw := doRaw(t, http.MethodPost, "/investigations/inv_missing/samples",
		`{"sample_id":"x","node":1,"result":"clean"}`)
	if status != http.StatusNotFound {
		t.Fatalf("unknown investigation status = %d, want 404", status)
	}
	assertErrorEnvelope(t, raw)

	// 422 cases: malformed payloads must leave no trace.
	for _, tc := range []struct {
		name string
		body string
	}{
		{"malformed json", `{`},
		{"missing sample_id", `{"node":1,"result":"clean"}`},
		{"empty sample_id", `{"sample_id":"","node":1,"result":"clean"}`},
		{"missing node", `{"sample_id":"a","result":"clean"}`},
		{"node wrong type", `{"sample_id":"a","node":"1","result":"clean"}`},
		{"bad result", `{"sample_id":"a","node":1,"result":"murky"}`},
		{"unknown field", `{"sample_id":"a","node":1,"result":"clean","x":1}`},
		{"node out of range", `{"sample_id":"a","node":9,"result":"clean"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, raw := doRaw(t, http.MethodPost, "/investigations/"+view.ID+"/samples", tc.body)
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (body %v)", status, raw)
			}
			assertErrorEnvelope(t, raw)
		})
	}

	// 409: non-target node (an inlet).
	status, raw = doRaw(t, http.MethodPost, "/investigations/"+view.ID+"/samples",
		`{"sample_id":"src","node":0,"result":"clean"}`)
	if status != http.StatusConflict {
		t.Fatalf("source node status = %d, want 409", status)
	}
	assertErrorEnvelope(t, raw)

	// 409: duplicate node.
	if status, _ := addSample(t, view.ID, "d1", 1, "clean"); status != http.StatusCreated {
		t.Fatalf("first add status = %d, want 201", status)
	}
	status, raw = doRaw(t, http.MethodPost, "/investigations/"+view.ID+"/samples",
		`{"sample_id":"d2","node":1,"result":"clean"}`)
	if status != http.StatusConflict {
		t.Fatalf("duplicate node status = %d, want 409", status)
	}
	assertErrorEnvelope(t, raw)

	// 409: duplicate sample id (still open; node 2 untouched).
	status, raw = doRaw(t, http.MethodPost, "/investigations/"+view.ID+"/samples",
		`{"sample_id":"d1","node":2,"result":"clean"}`)
	if status != http.StatusConflict {
		t.Fatalf("duplicate sample_id status = %d, want 409", status)
	}
	assertErrorEnvelope(t, raw)

	// The failed writes left no trace: node 2 is still open and id "d2"
	// can still be used.
	status, v := addSample(t, view.ID, "d2", 2, "clean")
	if status != http.StatusCreated || v.Status != "COMPLETED" {
		t.Fatalf("post-rejection add status=%d view=%+v, want 201 COMPLETED", status, v)
	}
}

func TestCreateInvestigationValidation422(t *testing.T) {
	resetInvestigationRepo()
	cases := []struct {
		name string
		body string
	}{
		{"malformed json", `{"n":2,"edges":[`},
		{"n too small", `{"n":1,"edges":[],"sources":[0]}`},
		{"n too large", `{"n":20001,"edges":[],"sources":[0]}`},
		{"n not an integer", `{"n":2.5,"edges":[],"sources":[0]}`},
		{"edge from missing", `{"n":2,"edges":[{"to":1}],"sources":[0]}`},
		{"edge to missing", `{"n":2,"edges":[{"from":0}],"sources":[0]}`},
		{"edge from out of range", `{"n":2,"edges":[{"from":2,"to":1}],"sources":[0]}`},
		{"edge to negative", `{"n":2,"edges":[{"from":0,"to":-1}],"sources":[0]}`},
		{"empty sources", `{"n":2,"edges":[],"sources":[]}`},
		{"missing sources", `{"n":2,"edges":[]}`},
		{"source out of range", `{"n":2,"edges":[],"sources":[2]}`},
		{"duplicate sources", `{"n":3,"edges":[],"sources":[0,0]}`},
		{"unknown field", `{"n":2,"edges":[],"sources":[0],"bogus":1}`},
		{"trailing data", `{"n":2,"edges":[],"sources":[0]} {}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, raw := doRaw(t, http.MethodPost, "/investigations", tc.body)
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (body %v)", status, raw)
			}
			assertErrorEnvelope(t, raw)
		})
	}

	// Rejected creation leaves no trace: the id space must be unaffected and
	// a subsequent valid creation still works.
	view := createInvestigation(t, `{"n":2,"edges":[{"from":0,"to":1}],"sources":[0]}`)
	if view.Status != "PENDING" {
		t.Fatalf("status = %s, want PENDING", view.Status)
	}
}

// TestConcurrentSamplingOverHTTP drives the real handler stack with many
// goroutines to prove the repository critical section records each target
// exactly once and closes the case exactly once.
func TestConcurrentSamplingOverHTTP(t *testing.T) {
	resetInvestigationRepo()
	const targets = 40
	var edges bytes.Buffer
	edges.WriteString(`{"n":41,"edges":[`)
	for v := 1; v <= targets; v++ {
		if v > 1 {
			edges.WriteByte(',')
		}
		fmt.Fprintf(&edges, `{"from":0,"to":%d}`, v)
	}
	edges.WriteString(`],"sources":[0]}`)
	view := createInvestigation(t, edges.String())

	const workers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted, completed, inProgress := 0, 0, 0
	nodes := make(map[int]bool, targets)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			handler := New()
			for node := 1; node <= targets; node++ {
				payload := fmt.Sprintf(`{"sample_id":"w%d-n%d","node":%d,"result":"clean"}`, w, node, node)
				req := httptest.NewRequest(http.MethodPost,
					"/investigations/"+view.ID+"/samples", bytes.NewBufferString(payload))
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				if rec.Code == http.StatusCreated {
					var v investigationView
					if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
						t.Errorf("decode: %v", err)
						return
					}
					mu.Lock()
					accepted++
					nodes[node] = true
					if v.Status == "COMPLETED" {
						completed++
					} else {
						inProgress++
					}
					mu.Unlock()
				} else if rec.Code != http.StatusConflict {
					t.Errorf("node %d: unexpected status %d", node, rec.Code)
				}
			}
		}(w)
	}
	close(start)
	wg.Wait()

	if accepted != targets {
		t.Fatalf("accepted = %d, want %d (no duplicates, no omissions)", accepted, targets)
	}
	if len(nodes) != targets {
		t.Fatalf("recorded nodes = %d, want %d", len(nodes), targets)
	}
	if completed != 1 {
		t.Fatalf("COMPLETED responses = %d, want exactly 1", completed)
	}
	if inProgress != targets-1 {
		t.Fatalf("IN_PROGRESS responses = %d, want %d", inProgress, targets-1)
	}

	// One more write after closure is 409.
	status, raw := doRaw(t, http.MethodPost, "/investigations/"+view.ID+"/samples",
		`{"sample_id":"late","node":1,"result":"contaminated"}`)
	if status != http.StatusConflict {
		t.Fatalf("post-close status = %d, want 409 (body %v)", status, raw)
	}
	assertErrorEnvelope(t, raw)
}

// TestConcurrentSameNodeAndID proves a duplicate race on one node/id has a
// single winner: only one request is accepted, the rest get 409.
func TestConcurrentSameNodeAndID(t *testing.T) {
	resetInvestigationRepo()
	view := createInvestigation(t, `{"n":2,"edges":[{"from":0,"to":1}],"sources":[0]}`)

	const racers = 16
	start := make(chan struct{})
	results := make([]int, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			req := httptest.NewRequest(http.MethodPost,
				"/investigations/"+view.ID+"/samples",
				bytes.NewBufferString(`{"sample_id":"same","node":1,"result":"clean"}`))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			New().ServeHTTP(rec, req)
			results[i] = rec.Code
		}(i)
	}
	close(start)
	wg.Wait()

	sort.Ints(results)
	if results[0] != http.StatusCreated {
		t.Fatalf("no winner among %d racer responses: %v", racers, results)
	}
	if results[1] != http.StatusConflict {
		t.Fatalf("responses = %v, want one 201 and %d 409s", results, racers-1)
	}
}

func assertErrorEnvelope(t *testing.T, raw map[string]any) {
	t.Helper()
	errObj, ok := raw["error"].(map[string]any)
	if !ok {
		t.Fatalf("error envelope missing: %v", raw)
	}
	if code, _ := errObj["code"].(string); code == "" {
		t.Fatalf("error.code missing/empty: %v", raw)
	}
	if msg, _ := errObj["message"].(string); msg == "" {
		t.Fatalf("error.message missing/empty: %v", raw)
	}
}
