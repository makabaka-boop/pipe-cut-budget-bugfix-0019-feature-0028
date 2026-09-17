package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"raincut/internal/investigation"
)

// Investigation input limits. Any violation is rejected with HTTP 422 and
// leaves no trace in the store.
const (
	minInvestigationNodes = 1
	maxInvestigationNodes = 20000
	maxInvestigationEdges = 100000
)

// investigationEdge is one directed pipe of the network snapshot.
type investigationEdge struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
}

// createInvestigationRequest is the JSON document accepted by
// POST /investigations. Nodes are numbered 0..n-1.
type createInvestigationRequest struct {
	N      int64               `json:"n"`
	Edges  []investigationEdge `json:"edges"`
	Inlets []int64             `json:"inlets"`
}

// addSampleRequest is the JSON document accepted by
// POST /investigations/{id}/samples. Node is a pointer so a missing field is
// distinguishable from node 0.
type addSampleRequest struct {
	SampleID string `json:"sample_id"`
	Node     *int64 `json:"node"`
	Result   string `json:"result"`
}

// investigationView is the full investigation document returned by both
// success responses.
type investigationView struct {
	ID         string                   `json:"id"`
	Status     investigation.Status     `json:"status"`
	Conclusion investigation.Conclusion `json:"conclusion"`
	Snapshot   snapshotView             `json:"snapshot"`
	Targets    []int64                  `json:"targets"`
	Samples    []sampleView             `json:"samples"`
	CreatedAt  time.Time                `json:"created_at"`
}

// snapshotView is the frozen network the investigation was created with.
type snapshotView struct {
	N      int64               `json:"n"`
	Edges  []investigationEdge `json:"edges"`
	Inlets []int64             `json:"inlets"`
}

type sampleView struct {
	SampleID string `json:"sample_id"`
	Node     int64  `json:"node"`
	Result   string `json:"result"`
}

func handleCreateInvestigation(store *investigation.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()

		var req createInvestigationRequest
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusUnprocessableEntity, "invalid_json",
				"body must be one JSON object with fields n, edges, inlets: "+err.Error())
			return
		}
		if dec.More() {
			writeError(w, http.StatusUnprocessableEntity, "invalid_json", "unexpected data after the JSON document")
			return
		}
		if msg := validateInvestigation(&req); msg != "" {
			writeError(w, http.StatusUnprocessableEntity, "invalid_graph", msg)
			return
		}

		edges := make([]investigation.Edge, len(req.Edges))
		for i, e := range req.Edges {
			edges[i] = investigation.Edge{From: e.From, To: e.To}
		}
		inv := store.Create(req.N, edges, req.Inlets, time.Now().UTC())
		writeJSON(w, http.StatusCreated, newInvestigationView(inv))
	}
}

func handleAddSample(store *investigation.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()

		var req addSampleRequest
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusUnprocessableEntity, "invalid_json",
				"body must be one JSON object with fields sample_id, node, result: "+err.Error())
			return
		}
		if dec.More() {
			writeError(w, http.StatusUnprocessableEntity, "invalid_json", "unexpected data after the JSON document")
			return
		}
		if msg := validateSample(&req); msg != "" {
			writeError(w, http.StatusUnprocessableEntity, "invalid_sample", msg)
			return
		}

		inv, err := store.AddSample(r.PathValue("id"), investigation.Sample{
			SampleID: req.SampleID,
			Node:     *req.Node,
			Result:   investigation.Result(req.Result),
		})
		if err != nil {
			writeInvestigationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, newInvestigationView(inv))
	}
}

// validateInvestigation checks every snapshot constraint and returns a
// human-readable message for the first violation, or "" when the request is
// acceptable.
func validateInvestigation(req *createInvestigationRequest) string {
	if req.N < minInvestigationNodes || req.N > maxInvestigationNodes {
		return fmt.Sprintf("n must be between %d and %d, got %d", minInvestigationNodes, maxInvestigationNodes, req.N)
	}
	n := req.N
	if len(req.Edges) > maxInvestigationEdges {
		return fmt.Sprintf("edges must contain at most %d entries, got %d", maxInvestigationEdges, len(req.Edges))
	}
	for i, e := range req.Edges {
		if e.From < 0 || e.From >= n {
			return fmt.Sprintf("edges[%d].from must be a node id in [0, %d], got %d", i, n-1, e.From)
		}
		if e.To < 0 || e.To >= n {
			return fmt.Sprintf("edges[%d].to must be a node id in [0, %d], got %d", i, n-1, e.To)
		}
	}
	if len(req.Inlets) == 0 {
		return "inlets must be a non-empty array of node ids"
	}
	for i, v := range req.Inlets {
		if v < 0 || v >= n {
			return fmt.Sprintf("inlets[%d] must be a node id in [0, %d], got %d", i, n-1, v)
		}
	}
	return ""
}

// validateSample checks the sample document itself; conflicts with stored
// state (duplicates, non-targets, terminal investigations) are left to the
// store so they are decided inside the critical section.
func validateSample(req *addSampleRequest) string {
	if req.SampleID == "" {
		return "sample_id must be a non-empty string"
	}
	if req.Node == nil {
		return "node must be a node id (integer >= 0)"
	}
	if *req.Node < 0 {
		return fmt.Sprintf("node must be a node id (integer >= 0), got %d", *req.Node)
	}
	if req.Result != string(investigation.ResultClean) && req.Result != string(investigation.ResultContaminated) {
		return fmt.Sprintf("result must be %q or %q, got %q",
			investigation.ResultClean, investigation.ResultContaminated, req.Result)
	}
	return ""
}

// writeInvestigationError maps store errors onto the stable error envelope.
func writeInvestigationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, investigation.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, investigation.ErrCompleted):
		writeError(w, http.StatusConflict, "investigation_completed", err.Error())
	case errors.Is(err, investigation.ErrDuplicateSampleID):
		writeError(w, http.StatusConflict, "duplicate_sample_id", err.Error())
	case errors.Is(err, investigation.ErrNodeAlreadySampled):
		writeError(w, http.StatusConflict, "node_already_sampled", err.Error())
	case errors.Is(err, investigation.ErrNodeNotTarget):
		writeError(w, http.StatusConflict, "node_not_target", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

// newInvestigationView renders the full investigation document. Empty
// collections are normalized so they serialize as [] rather than null.
func newInvestigationView(inv *investigation.Investigation) investigationView {
	edges := make([]investigationEdge, len(inv.Edges))
	for i, e := range inv.Edges {
		edges[i] = investigationEdge{From: e.From, To: e.To}
	}
	samples := make([]sampleView, len(inv.Samples))
	for i, s := range inv.Samples {
		samples[i] = sampleView{SampleID: s.SampleID, Node: s.Node, Result: string(s.Result)}
	}
	targets := inv.Targets
	if targets == nil {
		targets = []int64{}
	}
	inlets := inv.Inlets
	if inlets == nil {
		inlets = []int64{}
	}
	return investigationView{
		ID:         inv.ID,
		Status:     inv.Status,
		Conclusion: inv.Conclusion,
		Snapshot:   snapshotView{N: inv.N, Edges: edges, Inlets: inlets},
		Targets:    targets,
		Samples:    samples,
		CreatedAt:  inv.CreatedAt,
	}
}
