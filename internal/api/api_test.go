package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func do(t *testing.T, method, path, body string) (int, map[string]any) {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	rec := httptest.NewRecorder()
	New().ServeHTTP(rec, req)

	var parsed map[string]any
	if strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
			t.Fatalf("response is not valid JSON: %v (%q)", err, rec.Body.String())
		}
	}
	return rec.Code, parsed
}

func TestMinimumShutdownCostExactValues(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int64
	}{
		{
			"single pipe",
			`{"n":2,"edges":[{"from":0,"to":1,"cost":7}],"sources":[0],"sinks":[1]}`,
			7,
		},
		{
			// Both parallel pipes must be shut; each is billed separately.
			"parallel edges are billed individually",
			`{"n":3,"edges":[
				{"from":0,"to":1,"cost":3},
				{"from":0,"to":1,"cost":4},
				{"from":1,"to":2,"cost":10}],
			 "sources":[0],"sinks":[2]}`,
			7,
		},
		{
			// Self-loops can never cross a cut, so they must not change the answer.
			"self loops do not affect the result",
			`{"n":3,"edges":[
				{"from":0,"to":1,"cost":3},
				{"from":0,"to":1,"cost":4},
				{"from":1,"to":2,"cost":10},
				{"from":0,"to":0,"cost":1},
				{"from":1,"to":1,"cost":2},
				{"from":2,"to":2,"cost":100}],
			 "sources":[0],"sinks":[2]}`,
			7,
		},
		{
			// Two inlets share one bottleneck pipe: shutting pipe 2->3 (3)
			// disconnects both intakes.
			"multi source multi sink",
			`{"n":6,"edges":[
				{"from":0,"to":2,"cost":4},
				{"from":1,"to":2,"cost":4},
				{"from":2,"to":3,"cost":3},
				{"from":3,"to":4,"cost":4},
				{"from":3,"to":5,"cost":4}],
			 "sources":[0,1],"sinks":[4,5]}`,
			3,
		},
		{
			"no path from source to sink costs zero",
			`{"n":4,"edges":[{"from":0,"to":1,"cost":9},{"from":2,"to":3,"cost":9}],
			 "sources":[0],"sinks":[3]}`,
			0,
		},
		{
			"empty edge list costs zero",
			`{"n":2,"edges":[],"sources":[0],"sinks":[1]}`,
			0,
		},
		{
			// 3e9 exceeds int32: the answer must be a 64-bit integer.
			"64 bit answer",
			`{"n":2,"edges":[
				{"from":0,"to":1,"cost":1000000000},
				{"from":0,"to":1,"cost":1000000000},
				{"from":0,"to":1,"cost":1000000000}],
			 "sources":[0],"sinks":[1]}`,
			3_000_000_000,
		},
		{
			// The super links cost more than every pipe combined, so the cut
			// must price real pipes (5+7) and never a super link.
			"super links are never cut",
			`{"n":4,"edges":[
				{"from":0,"to":2,"cost":5},
				{"from":1,"to":3,"cost":7}],
			 "sources":[0,1],"sinks":[2,3]}`,
			12,
		},
		{
			"cut picks the cheapest side",
			`{"n":4,"edges":[
				{"from":0,"to":1,"cost":8},
				{"from":0,"to":2,"cost":3},
				{"from":1,"to":3,"cost":4},
				{"from":2,"to":3,"cost":10}],
			 "sources":[0],"sinks":[3]}`,
			7,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := do(t, http.MethodPost, "/minimum-shutdown-cost", tc.body)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %v)", status, body)
			}
			got, ok := body["minimum_shutdown_cost"].(float64)
			if !ok {
				t.Fatalf("response missing minimum_shutdown_cost: %v", body)
			}
			if int64(got) != tc.want {
				t.Fatalf("minimum_shutdown_cost = %v, want %d", got, tc.want)
			}
		})
	}
}

func TestInvalidGraphsReturnStableError(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"n below minimum", `{"n":1,"edges":[],"sources":[0],"sinks":[0]}`},
		{"n above maximum", `{"n":20001,"edges":[],"sources":[0],"sinks":[1]}`},
		{"n not an integer", `{"n":2.5,"edges":[],"sources":[0],"sinks":[1]}`},
		{"edge from out of range", `{"n":2,"edges":[{"from":2,"to":1,"cost":1}],"sources":[0],"sinks":[1]}`},
		{"edge to out of range", `{"n":2,"edges":[{"from":0,"to":-1,"cost":1}],"sources":[0],"sinks":[1]}`},
		{"cost zero", `{"n":2,"edges":[{"from":0,"to":1,"cost":0}],"sources":[0],"sinks":[1]}`},
		{"cost above maximum", `{"n":2,"edges":[{"from":0,"to":1,"cost":1000000001}],"sources":[0],"sinks":[1]}`},
		{"cost not an integer", `{"n":2,"edges":[{"from":0,"to":1,"cost":1.5}],"sources":[0],"sinks":[1]}`},
		{"cost wrong type", `{"n":2,"edges":[{"from":0,"to":1,"cost":"7"}],"sources":[0],"sinks":[1]}`},
		{"empty sources", `{"n":2,"edges":[],"sources":[],"sinks":[1]}`},
		{"missing sinks", `{"n":2,"edges":[],"sources":[0]}`},
		{"source sink overlap", `{"n":3,"edges":[],"sources":[0,1],"sinks":[1,2]}`},
		{"unknown field", `{"n":2,"edges":[],"sources":[0],"sinks":[1],"debug":true}`},
		{"malformed json", `{"n":2,"edges":[`},
		{"trailing data", `{"n":2,"edges":[],"sources":[0],"sinks":[1]} {}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := do(t, http.MethodPost, "/minimum-shutdown-cost", tc.body)
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (body %v)", status, body)
			}
			errObj, ok := body["error"].(map[string]any)
			if !ok {
				t.Fatalf("error envelope missing: %v", body)
			}
			if code, _ := errObj["code"].(string); code == "" {
				t.Fatalf("error.code missing or empty: %v", body)
			}
			if msg, _ := errObj["message"].(string); msg == "" {
				t.Fatalf("error.message missing or empty: %v", body)
			}
		})
	}
}

func TestTooManyEdgesRejected(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"n":2,"edges":[`)
	for i := 0; i <= maxEdges; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"from":0,"to":1,"cost":1}`)
	}
	b.WriteString(`],"sources":[0],"sinks":[1]}`)
	status, _ := do(t, http.MethodPost, "/minimum-shutdown-cost", b.String())
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 for %d edges", status, maxEdges+1)
	}
}

func TestMethodAndPathHandling(t *testing.T) {
	if status, _ := do(t, http.MethodGet, "/minimum-shutdown-cost", ""); status != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", status)
	}
	if status, _ := do(t, http.MethodPost, "/nope", `{}`); status != http.StatusNotFound {
		t.Fatalf("unknown path status = %d, want 404", status)
	}
	if status, _ := do(t, http.MethodGet, "/healthz", ""); status != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", status)
	}
}
