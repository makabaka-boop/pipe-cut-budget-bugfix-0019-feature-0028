package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"raincut/internal/investigation"
)

// investigationRepo backs the investigation endpoints. A single repository
// serializes every sample registration, which is what keeps concurrent
// sampling exactly-once.
var investigationRepo = investigation.NewRepository()

// investigationEdge is one directed pipe in a creation request. Pointers let
// validation distinguish a missing endpoint from a (valid) node id 0.
type investigationEdge struct {
	From *int64 `json:"from"`
	To   *int64 `json:"to"`
}

type createInvestigationRequest struct {
	N       int64               `json:"n"`
	Edges   []investigationEdge `json:"edges"`
	Sources []int64             `json:"sources"`
}

type addSampleRequest struct {
	SampleID string `json:"sample_id"`
	Node     *int64 `json:"node"`
	Result   string `json:"result"`
}

// edgeView is one directed pipe of the creation snapshot.
type edgeView struct {
	From int `json:"from"`
	To   int `json:"to"`
}

type sampleView struct {
	SampleID string `json:"sample_id"`
	Node     int    `json:"node"`
	Result   string `json:"result"`
}

// investigationResponse is the complete investigation view returned by both
// successful POST endpoints.
type investigationResponse struct {
	ID      string       `json:"id"`
	N       int          `json:"n"`
	Edges   []edgeView   `json:"edges"`
	Sources []int        `json:"sources"`
	Targets []int        `json:"targets"`
	Status  string       `json:"status"`
	Result  *string      `json:"result"`
	Samples []sampleView `json:"samples"`
}

func handleCreateInvestigation(w http.ResponseWriter, r *http.Request) {
	var req createInvestigationRequest
	if msg := decodeInvestigationBody(w, r, &req); msg != "" {
		writeError(w, "invalid_json", msg)
		return
	}
	if msg := validateCreate(&req); msg != "" {
		writeError(w, "invalid_investigation", msg)
		return
	}

	edges := make([]investigation.Edge, len(req.Edges))
	for i, e := range req.Edges {
		edges[i] = investigation.Edge{From: int(*e.From), To: int(*e.To)}
	}
	sources := make([]int, len(req.Sources))
	for i, s := range req.Sources {
		sources[i] = int(s)
	}

	view, err := investigationRepo.Create(int(req.N), edges, sources)
	if err != nil {
		writeErrorStatus(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, toInvestigationResponse(view))
}

func handleAddSample(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req addSampleRequest
	if msg := decodeInvestigationBody(w, r, &req); msg != "" {
		writeError(w, "invalid_json", msg)
		return
	}
	if msg := validateSample(&req); msg != "" {
		writeError(w, "invalid_sample", msg)
		return
	}

	view, err := investigationRepo.AddSample(id, req.SampleID, int(*req.Node), req.Result)
	switch {
	case err == nil:
		writeJSON(w, http.StatusCreated, toInvestigationResponse(view))
	case errors.Is(err, investigation.ErrNotFound):
		writeErrorStatus(w, http.StatusNotFound, "investigation_not_found",
			fmt.Sprintf("no investigation with id %q", id))
	case errors.Is(err, investigation.ErrNodeOutOfRange):
		var out *investigation.NodeOutOfRangeError
		_ = errors.As(err, &out)
		writeError(w, "invalid_sample",
			fmt.Sprintf("node must be a node id in [0, %d], got %d", out.N-1, out.Node))
	case errors.Is(err, investigation.ErrCompleted):
		writeErrorStatus(w, http.StatusConflict, "investigation_completed",
			"investigation is already COMPLETED; its state never regresses")
	case errors.Is(err, investigation.ErrDuplicateSampleID):
		writeErrorStatus(w, http.StatusConflict, "duplicate_sample_id",
			fmt.Sprintf("sample_id %q was already used; sample ids are globally unique", req.SampleID))
	case errors.Is(err, investigation.ErrNodeNotTarget):
		writeErrorStatus(w, http.StatusConflict, "node_not_target",
			fmt.Sprintf("node %d is not reachable from any pollution inlet and is not a sampling target", *req.Node))
	case errors.Is(err, investigation.ErrDuplicateNode):
		writeErrorStatus(w, http.StatusConflict, "node_already_sampled",
			fmt.Sprintf("node %d has already been sampled in this investigation", *req.Node))
	default:
		writeErrorStatus(w, http.StatusInternalServerError, "internal_error", err.Error())
	}
}

// decodeInvestigationBody parses one strict JSON object, mirroring the
// minimum-shutdown-cost decoding: bounded body, no unknown fields, exactly
// one document. It returns a 422 message on failure.
func decodeInvestigationBody(w http.ResponseWriter, r *http.Request, dst any) string {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return "body must be one JSON object: " + err.Error()
	}
	if dec.More() {
		return "unexpected data after the JSON document"
	}
	return ""
}

func validateCreate(req *createInvestigationRequest) string {
	if req.N < investigation.MinNodes || req.N > investigation.MaxNodes {
		return fmt.Sprintf("n must be between %d and %d, got %d",
			investigation.MinNodes, investigation.MaxNodes, req.N)
	}
	n := req.N
	if len(req.Edges) > investigation.MaxEdges {
		return fmt.Sprintf("edges must contain at most %d entries, got %d",
			investigation.MaxEdges, len(req.Edges))
	}
	for i, e := range req.Edges {
		if e.From == nil {
			return fmt.Sprintf("edges[%d].from is required and must be a node id", i)
		}
		if e.To == nil {
			return fmt.Sprintf("edges[%d].to is required and must be a node id", i)
		}
		if *e.From < 0 || *e.From >= n {
			return fmt.Sprintf("edges[%d].from must be a node id in [0, %d], got %d", i, n-1, *e.From)
		}
		if *e.To < 0 || *e.To >= n {
			return fmt.Sprintf("edges[%d].to must be a node id in [0, %d], got %d", i, n-1, *e.To)
		}
	}
	if len(req.Sources) == 0 {
		return "sources must be a non-empty array of node ids"
	}
	seen := make(map[int64]struct{}, len(req.Sources))
	for i, v := range req.Sources {
		if v < 0 || v >= n {
			return fmt.Sprintf("sources[%d] must be a node id in [0, %d], got %d", i, n-1, v)
		}
		if _, dup := seen[v]; dup {
			return fmt.Sprintf("sources[%d] duplicates pollution inlet %d; inlets must be unique", i, v)
		}
		seen[v] = struct{}{}
	}
	return ""
}

func validateSample(req *addSampleRequest) string {
	if req.SampleID == "" {
		return "sample_id is required and must be a non-empty string"
	}
	if req.Node == nil {
		return "node is required and must be a node id"
	}
	switch req.Result {
	case "clean", "contaminated":
	default:
		return fmt.Sprintf("result must be either %q or %q, got %q", "clean", "contaminated", req.Result)
	}
	return ""
}

func toInvestigationResponse(v investigation.View) investigationResponse {
	edges := make([]edgeView, len(v.Edges))
	for i, e := range v.Edges {
		edges[i] = edgeView{From: e.From, To: e.To}
	}
	samples := make([]sampleView, len(v.Samples))
	for i, s := range v.Samples {
		samples[i] = sampleView{SampleID: s.SampleID, Node: s.Node, Result: s.Verdict}
	}
	resp := investigationResponse{
		ID:      v.ID,
		N:       v.N,
		Edges:   edges,
		Sources: append([]int(nil), v.Sources...),
		Targets: append([]int(nil), v.Targets...),
		Status:  string(v.Status),
		Samples: samples,
	}
	if v.Result != "" {
		result := string(v.Result)
		resp.Result = &result
	}
	return resp
}
