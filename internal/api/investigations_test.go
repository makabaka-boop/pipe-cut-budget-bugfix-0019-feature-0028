package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// doOn issues a request against the given handler, so a test can share one
// store across several requests.
func doOn(t *testing.T, h http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var parsed map[string]any
	if strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
			t.Fatalf("response is not valid JSON: %v (%q)", err, rec.Body.String())
		}
	}
	return rec.Code, parsed
}

func mustCreate(t *testing.T, h http.Handler, body string) map[string]any {
	t.Helper()
	status, view := doOn(t, h, http.MethodPost, "/investigations", body)
	if status != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (body %v)", status, view)
	}
	return view
}

func mustSample(t *testing.T, h http.Handler, id, body string) map[string]any {
	t.Helper()
	status, view := doOn(t, h, http.MethodPost, "/investigations/"+id+"/samples", body)
	if status != http.StatusOK {
		t.Fatalf("sample status = %d, want 200 (body %v)", status, view)
	}
	return view
}

func errCode(t *testing.T, body map[string]any) string {
	t.Helper()
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("error envelope missing: %v", body)
	}
	code, _ := errObj["code"].(string)
	if code == "" {
		t.Fatalf("error.code missing or empty: %v", body)
	}
	if msg, _ := errObj["message"].(string); msg == "" {
		t.Fatalf("error.message missing or empty: %v", body)
	}
	return code
}

func TestCreateInvestigationComputesTargets(t *testing.T) {
	h := New()
	view := mustCreate(t, h, `{"n":6,"edges":[
		{"from":0,"to":2},{"from":0,"to":2},
		{"from":2,"to":3},{"from":3,"to":0},
		{"from":1,"to":3},{"from":3,"to":3},
		{"from":0,"to":0},{"from":5,"to":4}],
		"inlets":[0,1]}`)

	if view["id"] != "1" {
		t.Fatalf("id = %v, want 1", view["id"])
	}
	if view["status"] != "PENDING" {
		t.Fatalf("status = %v, want PENDING", view["status"])
	}
	if view["conclusion"] != "clean" {
		t.Fatalf("conclusion = %v, want clean", view["conclusion"])
	}
	if got := fmt.Sprint(view["targets"]); got != "[2 3]" {
		t.Fatalf("targets = %s, want [2 3]", got)
	}
	if samples, ok := view["samples"].([]any); !ok || len(samples) != 0 {
		t.Fatalf("samples = %v, want empty array", view["samples"])
	}
	snapshot, ok := view["snapshot"].(map[string]any)
	if !ok {
		t.Fatalf("snapshot missing: %v", view)
	}
	if snapshot["n"] != float64(6) {
		t.Fatalf("snapshot.n = %v, want 6", snapshot["n"])
	}
	if got := fmt.Sprint(snapshot["inlets"]); got != "[0 1]" {
		t.Fatalf("snapshot.inlets = %s, want [0 1]", got)
	}
	if edges, ok := snapshot["edges"].([]any); !ok || len(edges) != 8 {
		t.Fatalf("snapshot.edges = %v, want 8 entries", snapshot["edges"])
	}
	if _, ok := view["created_at"].(string); !ok {
		t.Fatalf("created_at missing: %v", view)
	}
}

func TestCreateInvestigationWithoutTargetsCompletesImmediately(t *testing.T) {
	h := New()
	view := mustCreate(t, h, `{"n":2,"edges":[{"from":0,"to":0}],"inlets":[0]}`)
	if view["status"] != "COMPLETED" {
		t.Fatalf("status = %v, want COMPLETED", view["status"])
	}
	if targets, ok := view["targets"].([]any); !ok || len(targets) != 0 {
		t.Fatalf("targets = %v, want empty array", view["targets"])
	}
	if view["conclusion"] != "clean" {
		t.Fatalf("conclusion = %v, want clean", view["conclusion"])
	}

	// Terminal from birth: writes are rejected with 409.
	status, body := doOn(t, h, http.MethodPost, "/investigations/1/samples",
		`{"sample_id":"late","node":0,"result":"clean"}`)
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %v)", status, body)
	}
	if code := errCode(t, body); code != "investigation_completed" {
		t.Fatalf("code = %s, want investigation_completed", code)
	}
}

func TestInvestigationLifecycleAndConclusion(t *testing.T) {
	h := New()
	view := mustCreate(t, h, `{"n":4,"edges":[{"from":0,"to":1},{"from":1,"to":2}],"inlets":[0]}`)
	id := view["id"].(string)

	view = mustSample(t, h, id, `{"sample_id":"a","node":1,"result":"clean"}`)
	if view["status"] != "IN_PROGRESS" {
		t.Fatalf("status = %v, want IN_PROGRESS", view["status"])
	}
	if view["conclusion"] != "clean" {
		t.Fatalf("conclusion = %v, want clean", view["conclusion"])
	}
	if samples := view["samples"].([]any); len(samples) != 1 {
		t.Fatalf("samples = %v, want 1 entry", samples)
	}

	view = mustSample(t, h, id, `{"sample_id":"b","node":2,"result":"contaminated"}`)
	if view["status"] != "COMPLETED" {
		t.Fatalf("status = %v, want COMPLETED", view["status"])
	}
	if view["conclusion"] != "contaminated" {
		t.Fatalf("conclusion = %v, want contaminated", view["conclusion"])
	}
	if samples := view["samples"].([]any); len(samples) != 2 {
		t.Fatalf("samples = %v, want 2 entries", samples)
	}

	// Terminal state does not regress.
	status, body := doOn(t, h, http.MethodPost, "/investigations/"+id+"/samples",
		`{"sample_id":"c","node":1,"result":"clean"}`)
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %v)", status, body)
	}
	if code := errCode(t, body); code != "investigation_completed" {
		t.Fatalf("code = %s, want investigation_completed", code)
	}
}

func TestInvestigationAllCleanConcludesClean(t *testing.T) {
	h := New()
	view := mustCreate(t, h, `{"n":3,"edges":[{"from":0,"to":1},{"from":1,"to":2}],"inlets":[0]}`)
	id := view["id"].(string)
	mustSample(t, h, id, `{"sample_id":"a","node":1,"result":"clean"}`)
	view = mustSample(t, h, id, `{"sample_id":"b","node":2,"result":"clean"}`)
	if view["status"] != "COMPLETED" || view["conclusion"] != "clean" {
		t.Fatalf("got (%v, %v), want (COMPLETED, clean)", view["status"], view["conclusion"])
	}
}

func TestSampleConflictsReturn409(t *testing.T) {
	h := New()
	view := mustCreate(t, h, `{"n":5,"edges":[{"from":0,"to":1},{"from":0,"to":2}],"inlets":[0]}`)
	id := view["id"].(string)
	mustSample(t, h, id, `{"sample_id":"first","node":1,"result":"clean"}`)

	cases := []struct {
		name string
		body string
		code string
	}{
		{"duplicate sample_id", `{"sample_id":"first","node":2,"result":"clean"}`, "duplicate_sample_id"},
		{"node already sampled", `{"sample_id":"second","node":1,"result":"clean"}`, "node_already_sampled"},
		{"node not a target", `{"sample_id":"third","node":3,"result":"clean"}`, "node_not_target"},
		{"node out of range", `{"sample_id":"fourth","node":9,"result":"clean"}`, "node_not_target"},
		{"inlet is not a target", `{"sample_id":"fifth","node":0,"result":"clean"}`, "node_not_target"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := doOn(t, h, http.MethodPost, "/investigations/"+id+"/samples", tc.body)
			if status != http.StatusConflict {
				t.Fatalf("status = %d, want 409 (body %v)", status, body)
			}
			if code := errCode(t, body); code != tc.code {
				t.Fatalf("code = %s, want %s", code, tc.code)
			}
		})
	}

	// Conflicts leave no trace: the remaining target still completes the
	// investigation with exactly two samples.
	view = mustSample(t, h, id, `{"sample_id":"last","node":2,"result":"clean"}`)
	if samples := view["samples"].([]any); len(samples) != 2 {
		t.Fatalf("samples = %v, want exactly 2 entries", samples)
	}
	if view["status"] != "COMPLETED" {
		t.Fatalf("status = %v, want COMPLETED", view["status"])
	}
}

func TestSampleOnUnknownInvestigationReturns404(t *testing.T) {
	h := New()
	status, body := doOn(t, h, http.MethodPost, "/investigations/9999/samples",
		`{"sample_id":"a","node":1,"result":"clean"}`)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %v)", status, body)
	}
	if code := errCode(t, body); code != "not_found" {
		t.Fatalf("code = %s, want not_found", code)
	}
}

func TestInvalidInvestigationRequestsReturn422(t *testing.T) {
	h := New()
	creates := []struct {
		name string
		body string
	}{
		{"n zero", `{"n":0,"edges":[],"inlets":[0]}`},
		{"n above maximum", `{"n":20001,"edges":[],"inlets":[0]}`},
		{"edge from out of range", `{"n":2,"edges":[{"from":2,"to":1}],"inlets":[0]}`},
		{"edge to negative", `{"n":2,"edges":[{"from":0,"to":-1}],"inlets":[0]}`},
		{"empty inlets", `{"n":2,"edges":[],"inlets":[]}`},
		{"missing inlets", `{"n":2,"edges":[]}`},
		{"inlet out of range", `{"n":2,"edges":[],"inlets":[2]}`},
		{"n not an integer", `{"n":2.5,"edges":[],"inlets":[0]}`},
		{"unknown field", `{"n":2,"edges":[],"inlets":[0],"debug":true}`},
		{"malformed json", `{"n":2,"edges":[`},
		{"trailing data", `{"n":2,"edges":[],"inlets":[0]} {}`},
	}
	for _, tc := range creates {
		t.Run("create: "+tc.name, func(t *testing.T) {
			status, body := doOn(t, h, http.MethodPost, "/investigations", tc.body)
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (body %v)", status, body)
			}
			errCode(t, body)
		})
	}

	view := mustCreate(t, h, `{"n":3,"edges":[{"from":0,"to":1},{"from":0,"to":2}],"inlets":[0]}`)
	id := view["id"].(string)
	samples := []struct {
		name string
		body string
	}{
		{"missing sample_id", `{"node":1,"result":"clean"}`},
		{"empty sample_id", `{"sample_id":"","node":1,"result":"clean"}`},
		{"missing node", `{"sample_id":"a","result":"clean"}`},
		{"negative node", `{"sample_id":"a","node":-1,"result":"clean"}`},
		{"node not an integer", `{"sample_id":"a","node":1.5,"result":"clean"}`},
		{"unknown result", `{"sample_id":"a","node":1,"result":"murky"}`},
		{"missing result", `{"sample_id":"a","node":1}`},
		{"unknown field", `{"sample_id":"a","node":1,"result":"clean","note":"x"}`},
		{"malformed json", `{"sample_id":"a",`},
	}
	for _, tc := range samples {
		t.Run("sample: "+tc.name, func(t *testing.T) {
			status, body := doOn(t, h, http.MethodPost, "/investigations/"+id+"/samples", tc.body)
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (body %v)", status, body)
			}
			errCode(t, body)
		})
	}

	// The invalid writes left no trace: both targets are still open and the
	// investigation completes with exactly the valid samples.
	mustSample(t, h, id, `{"sample_id":"ok-1","node":1,"result":"clean"}`)
	view = mustSample(t, h, id, `{"sample_id":"ok-2","node":2,"result":"clean"}`)
	if samples := view["samples"].([]any); len(samples) != 2 {
		t.Fatalf("samples = %v, want exactly 2 entries", samples)
	}
	if view["status"] != "COMPLETED" {
		t.Fatalf("status = %v, want COMPLETED", view["status"])
	}
}

func TestInvestigationIDsAreUniqueAcrossCreations(t *testing.T) {
	h := New()
	first := mustCreate(t, h, `{"n":2,"edges":[{"from":0,"to":1}],"inlets":[0]}`)
	second := mustCreate(t, h, `{"n":2,"edges":[{"from":0,"to":1}],"inlets":[0]}`)
	if first["id"] == second["id"] {
		t.Fatalf("ids must differ, both are %v", first["id"])
	}

	// sample_id uniqueness is global across investigations.
	mustSample(t, h, first["id"].(string), `{"sample_id":"shared","node":1,"result":"clean"}`)
	status, body := doOn(t, h, http.MethodPost, "/investigations/"+second["id"].(string)+"/samples",
		`{"sample_id":"shared","node":1,"result":"clean"}`)
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %v)", status, body)
	}
	if code := errCode(t, body); code != "duplicate_sample_id" {
		t.Fatalf("code = %s, want duplicate_sample_id", code)
	}
}
