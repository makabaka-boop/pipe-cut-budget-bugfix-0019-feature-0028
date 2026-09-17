package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// runPrefix scopes freshly generated sample ids to one verify run so the
// client is repeatable against a long-lived server: sample ids are globally
// unique for the lifetime of the service. The duplicate-id checks reuse the
// same prefixed value within the run, which is exactly what they test.
var runPrefix = newRunPrefix()

func newRunPrefix() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "run-"
	}
	return "run-" + hex.EncodeToString(b[:]) + "-"
}

// sid returns a run-unique sample id for the given logical name.
func sid(name string) string { return runPrefix + name }

type investigationEdge struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
}

type createInvestigationReq struct {
	N       int64               `json:"n"`
	Edges   []investigationEdge `json:"edges"`
	Sources []int64             `json:"sources"`
}

type addSampleReq struct {
	SampleID string `json:"sample_id"`
	Node     int64  `json:"node"`
	Result   string `json:"result"`
}

type sampleDoc struct {
	SampleID string `json:"sample_id"`
	Node     int    `json:"node"`
	Result   string `json:"result"`
}

// investigationDoc is the complete view returned by both successful
// investigation endpoints.
type investigationDoc struct {
	ID    string `json:"id"`
	N     int    `json:"n"`
	Edges []struct {
		From int `json:"from"`
		To   int `json:"to"`
	} `json:"edges"`
	Sources []int       `json:"sources"`
	Targets []int       `json:"targets"`
	Status  string      `json:"status"`
	Result  *string     `json:"result"`
	Samples []sampleDoc `json:"samples"`
}

func checkInvestigations() {
	checkTargetSets()
	checkNoTargetCompletedAtCreation()
	checkInvestigationLifecycle("contaminated conclusion", "contaminated")
	checkInvestigationLifecycle("clean conclusion", "clean")
	checkInvestigationErrors()
	checkConcurrentSampling()
	checkConcurrentSameIdRace()
}

// checkTargetSets verifies against a live server that the reachable target
// set is computed by iterative traversal over directed edges, excluding the
// inlets themselves, and that cycles, self-loops and parallel edges never
// duplicate a target.
func checkTargetSets() {
	// 0->1 twice (parallel), 1->1 (self-loop), 1<->2 (cycle back to the
	// inlet's side), 2->3, 3->4. Every reachable node appears exactly once.
	req := createInvestigationReq{
		N: 5,
		Edges: []investigationEdge{
			{0, 1}, {0, 1}, {1, 1}, {1, 2}, {2, 1}, {2, 3}, {3, 4},
		},
		Sources: []int64{0},
	}
	view, status := mustCreate("target set: cycles, self-loops, parallel edges", req)
	if status != http.StatusCreated {
		return
	}
	assertTargets("target set: cycles, self-loops, parallel edges", view, []int{1, 2, 3, 4})
	if view.Status != "PENDING" || view.Result != nil || len(view.Samples) != 0 {
		record("target set: initial view",
			fmt.Errorf("got status=%s result=%v samples=%d, want PENDING/nil/0",
				view.Status, view.Result, len(view.Samples)))
		return
	}
	pass(fmt.Sprintf("target set: cycles, self-loops, parallel edges -> %v", view.Targets))

	// Two inlets; a node reachable from both is listed once, and neither
	// inlet is ever a target even when one inlet can reach the other.
	req = createInvestigationReq{
		N: 6,
		Edges: []investigationEdge{
			{0, 1}, {1, 2}, {2, 3}, {4, 3}, {3, 5}, {2, 0},
		},
		Sources: []int64{0, 4},
	}
	view, status = mustCreate("target set: multiple inlets", req)
	if status != http.StatusCreated {
		return
	}
	assertTargets("target set: multiple inlets", view, []int{1, 2, 3, 5})

	// Directed graph: an edge pointing "backwards" must not make the source
	// side reachable.
	req = createInvestigationReq{N: 3, Edges: []investigationEdge{{1, 0}, {1, 2}}, Sources: []int64{0}}
	view, status = mustCreate("target set: edge direction is respected", req)
	if status != http.StatusCreated {
		return
	}
	assertTargets("target set: edge direction is respected", view, []int{})
}

// checkNoTargetCompletedAtCreation: with no reachable target the
// investigation exists COMPLETED/clean immediately, and no sample can be
// attached afterwards.
func checkNoTargetCompletedAtCreation() {
	name := "no targets: created COMPLETED/clean"
	req := createInvestigationReq{N: 4, Edges: []investigationEdge{{2, 3}}, Sources: []int64{0}}
	view, status := mustCreate(name, req)
	if status != http.StatusCreated {
		return
	}
	if view.Status != "COMPLETED" || view.Result == nil || *view.Result != "clean" {
		record(name, fmt.Errorf("got status=%s result=%v, want COMPLETED/clean", view.Status, view.Result))
		return
	}
	body, _ := json.Marshal(addSampleReq{SampleID: sid("late-1"), Node: 2, Result: "clean"})
	st, doc, _, err := postPath(http.MethodPost, samplePath(view.ID), 10*time.Second, body)
	switch {
	case err != nil:
		record(name, err)
	case st != http.StatusConflict:
		record(name, fmt.Errorf("post-completion sample status = %d, want 409", st))
	case envelopeShape(doc) == "":
		record(name, fmt.Errorf("409 without error envelope: %v", doc))
	default:
		pass(name + " and terminal writes are 409")
	}
}

// checkInvestigationLifecycle walks PENDING -> IN_PROGRESS -> COMPLETED
// through the real server, checks that both successful responses carry the
// complete view, that the terminal state never regresses and that the
// conclusion matches the recorded verdicts.
func checkInvestigationLifecycle(name, finalVerdict string) {
	// Path 0->1->2->3, three targets.
	req := createInvestigationReq{
		N:       4,
		Edges:   []investigationEdge{{0, 1}, {1, 2}, {2, 3}},
		Sources: []int64{0},
	}
	view, status := mustCreate(name, req)
	if status != http.StatusCreated {
		return
	}
	id := view.ID

	// First sample: PENDING -> IN_PROGRESS.
	view, st := mustAddSample(name, id, addSampleReq{SampleID: sid(name + "-s1"), Node: 1, Result: "clean"})
	if st != http.StatusCreated {
		return
	}
	if view.Status != "IN_PROGRESS" || view.Result != nil || len(view.Samples) != 1 {
		record(name, fmt.Errorf("after first sample: status=%s result=%v samples=%d, want IN_PROGRESS/nil/1",
			view.Status, view.Result, len(view.Samples)))
		return
	}

	// Second sample keeps it open; one contaminated verdict decides the
	// conclusion for the contaminated case.
	secondVerdict := "clean"
	if finalVerdict == "contaminated" {
		secondVerdict = "contaminated"
	}
	view, st = mustAddSample(name, id, addSampleReq{SampleID: sid(name + "-s2"), Node: 3, Result: secondVerdict})
	if st != http.StatusCreated {
		return
	}
	if view.Status != "IN_PROGRESS" || view.Result != nil {
		record(name, fmt.Errorf("after second sample: status=%s result=%v, want IN_PROGRESS/nil",
			view.Status, view.Result))
		return
	}

	// Last open target closes the case exactly once.
	view, st = mustAddSample(name, id, addSampleReq{SampleID: sid(name + "-s3"), Node: 2, Result: "clean"})
	if st != http.StatusCreated {
		return
	}
	if view.Status != "COMPLETED" || view.Result == nil || *view.Result != finalVerdict {
		record(name, fmt.Errorf("final: status=%s result=%v, want COMPLETED/%s",
			view.Status, view.Result, finalVerdict))
		return
	}
	if len(view.Samples) != 3 {
		record(name, fmt.Errorf("samples = %d, want 3", len(view.Samples)))
		return
	}
	// Samples come back ordered by node.
	for i, node := range []int{1, 2, 3} {
		if view.Samples[i].Node != node {
			record(name, fmt.Errorf("samples not ordered by node: %+v", view.Samples))
			return
		}
	}

	// Terminal state never regresses: even a fresh id/node is 409 and the
	// stored conclusion is unchanged.
	body, _ := json.Marshal(addSampleReq{SampleID: sid(name + "-after"), Node: 1, Result: "contaminated"})
	st, doc, _, err := postPath(http.MethodPost, samplePath(id), 10*time.Second, body)
	switch {
	case err != nil:
		record(name, err)
	case st != http.StatusConflict:
		record(name, fmt.Errorf("terminal write status = %d, want 409", st))
	case envelopeShape(doc) == "":
		record(name, fmt.Errorf("409 without error envelope: %v", doc))
	default:
		pass(fmt.Sprintf("%s: PENDING->IN_PROGRESS->COMPLETED, conclusion=%s, terminal writes 409",
			name, finalVerdict))
	}
}

// checkInvestigationErrors verifies the status matrix against the live API:
// 422 for malformed input (which leaves no trace), 404 for unknown
// investigations, 409 for duplicate ids/nodes, non-target nodes and terminal
// writes. Every error keeps the {"error":{"code","message"}} envelope.
func checkInvestigationErrors() {
	invalidCreate := []struct {
		name string
		body []byte
	}{
		{"malformed json", []byte(`{"n":2,"edges":[`)},
		{"n out of range", []byte(`{"n":1,"edges":[],"sources":[0]}`)},
		{"edge endpoint out of range", []byte(`{"n":2,"edges":[{"from":0,"to":2}],"sources":[0]}`)},
		{"missing edge endpoint", []byte(`{"n":2,"edges":[{"from":0}],"sources":[0]}`)},
		{"empty sources", []byte(`{"n":2,"edges":[],"sources":[]}`)},
		{"duplicate sources", []byte(`{"n":3,"edges":[],"sources":[0,0]}`)},
		{"unknown field", []byte(`{"n":2,"edges":[],"sources":[0],"x":1}`)},
	}
	for _, tc := range invalidCreate {
		st, doc, _, err := postPath(http.MethodPost, "/investigations", 10*time.Second, tc.body)
		switch {
		case err != nil:
			record("investigation 422: create "+tc.name, err)
		case st != http.StatusUnprocessableEntity:
			record("investigation 422: create "+tc.name, fmt.Errorf("status = %d, want 422", st))
		case envelopeShape(doc) == "":
			record("investigation 422: create "+tc.name, fmt.Errorf("bad envelope: %v", doc))
		default:
			pass("investigation 422: create " + tc.name)
		}
	}

	// Unknown investigation -> 404, even with an otherwise valid body.
	body, _ := json.Marshal(addSampleReq{SampleID: sid("ghost"), Node: 1, Result: "clean"})
	st, doc, _, err := postPath(http.MethodPost, "/investigations/inv_does_not_exist/samples", 10*time.Second, body)
	switch {
	case err != nil:
		record("investigation 404: unknown id", err)
	case st != http.StatusNotFound:
		record("investigation 404: unknown id", fmt.Errorf("status = %d, want 404", st))
	case envelopeShape(doc) == "":
		record("investigation 404: unknown id", fmt.Errorf("bad envelope: %v", doc))
	default:
		pass("investigation 404: unknown investigation id")
	}

	// Open investigation with targets 1,2.
	view, status := mustCreate("error matrix", createInvestigationReq{
		N:       3,
		Edges:   []investigationEdge{{0, 1}, {1, 2}},
		Sources: []int64{0},
	})
	if status != http.StatusCreated {
		return
	}
	id := view.ID

	expect422 := func(label string, payload []byte) {
		st, doc, _, err := postPath(http.MethodPost, samplePath(id), 10*time.Second, payload)
		switch {
		case err != nil:
			record(label, err)
		case st != http.StatusUnprocessableEntity:
			record(label, fmt.Errorf("status = %d, want 422", st))
		case envelopeShape(doc) == "":
			record(label, fmt.Errorf("bad envelope: %v", doc))
		default:
			pass(label)
		}
	}
	expect422("investigation 422: malformed sample json", []byte(`{`))
	expect422("investigation 422: missing sample_id", []byte(`{"node":1,"result":"clean"}`))
	expect422("investigation 422: missing node", []byte(`{"sample_id":"a","result":"clean"}`))
	expect422("investigation 422: bad result", []byte(`{"sample_id":"a","node":1,"result":"murky"}`))
	expect422("investigation 422: node out of range", []byte(`{"sample_id":"a","node":9,"result":"clean"}`))
	expect422("investigation 422: node wrong type", []byte(`{"sample_id":"a","node":"1","result":"clean"}`))

	expect409 := func(label string, payload []byte) {
		st, doc, _, err := postPath(http.MethodPost, samplePath(id), 10*time.Second, payload)
		switch {
		case err != nil:
			record(label, err)
		case st != http.StatusConflict:
			record(label, fmt.Errorf("status = %d, want 409", st))
		case envelopeShape(doc) == "":
			record(label, fmt.Errorf("bad envelope: %v", doc))
		default:
			pass(label)
		}
	}
	expect409("investigation 409: non-target (inlet) node",
		marshalSample(addSampleReq{SampleID: sid("src"), Node: 0, Result: "clean"}))

	// First valid sample at node 1.
	if _, st := mustAddSample("error matrix", id, addSampleReq{SampleID: sid("dup-check"), Node: 1, Result: "clean"}); st != http.StatusCreated {
		return
	}
	expect409("investigation 409: duplicate node",
		marshalSample(addSampleReq{SampleID: sid("other"), Node: 1, Result: "clean"}))
	expect409("investigation 409: duplicate sample_id",
		marshalSample(addSampleReq{SampleID: sid("dup-check"), Node: 2, Result: "clean"}))

	// sample_id is globally unique: it cannot be reused in another
	// investigation either.
	other, st2 := mustCreate("error matrix second", createInvestigationReq{
		N: 2, Edges: []investigationEdge{{0, 1}}, Sources: []int64{0},
	})
	if st2 != http.StatusCreated {
		return
	}
	expect409("investigation 409: sample_id reused in another investigation",
		marshalSample(addSampleReq{SampleID: sid("dup-check"), Node: 1, Result: "clean"}))
	_ = other

	// The 422/409 rejections left no trace: "other" and node 2 are still
	// usable, and this last sample closes the first investigation clean.
	view, st = mustAddSample("error matrix: rejected writes left no trace", id,
		addSampleReq{SampleID: sid("other"), Node: 2, Result: "clean"})
	if st != http.StatusCreated {
		return
	}
	if view.Status != "COMPLETED" || view.Result == nil || *view.Result != "clean" {
		record("error matrix: rejected writes left no trace",
			fmt.Errorf("got status=%s result=%v, want COMPLETED/clean", view.Status, view.Result))
		return
	}
	expect409("investigation 409: terminal write",
		marshalSample(addSampleReq{SampleID: sid("too-late"), Node: 1, Result: "clean"}))
}

// checkConcurrentSampling fires many clients at one investigation: every
// target must be recorded exactly once (no duplicates, no omissions) and
// COMPLETED must be returned exactly once.
func checkConcurrentSampling() {
	const targets = 50
	name := "concurrent completion: exact records, one close"

	edges := make([]investigationEdge, 0, targets)
	for v := int64(1); v <= targets; v++ {
		edges = append(edges, investigationEdge{0, v})
	}
	view, status := mustCreate(name, createInvestigationReq{N: targets + 1, Edges: edges, Sources: []int64{0}})
	if status != http.StatusCreated {
		return
	}
	id := view.ID

	const workers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted, completed, inProgress, unexpected := 0, 0, 0, 0
	recorded := make(map[int]bool, targets)
	start := make(chan struct{})
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			client := &http.Client{Timeout: 30 * time.Second}
			<-start
			for node := 1; node <= targets; node++ {
				payload, _ := json.Marshal(addSampleReq{
					SampleID: sid(fmt.Sprintf("cc-w%d-n%d", w, node)),
					Node:     int64(node),
					Result:   "clean",
				})
				req, _ := http.NewRequest(http.MethodPost, apiURL+samplePath(id), bytes.NewReader(payload))
				req.Header.Set("Content-Type", "application/json")
				res, err := client.Do(req)
				if err != nil {
					mu.Lock()
					unexpected++
					mu.Unlock()
					continue
				}
				code := res.StatusCode
				var doc investigationDoc
				_ = json.NewDecoder(res.Body).Decode(&doc)
				_ = res.Body.Close()
				mu.Lock()
				switch code {
				case http.StatusCreated:
					accepted++
					recorded[node] = true
					if doc.Status == "COMPLETED" {
						completed++
					} else {
						inProgress++
					}
				case http.StatusConflict:
					// Expected for losing racers.
				default:
					unexpected++
				}
				mu.Unlock()
			}
		}(w)
	}
	close(start)
	wg.Wait()

	switch {
	case unexpected != 0:
		record(name, fmt.Errorf("%d unexpected responses", unexpected))
	case accepted != targets:
		record(name, fmt.Errorf("accepted samples = %d, want exactly %d", accepted, targets))
	case len(recorded) != targets:
		record(name, fmt.Errorf("recorded nodes = %d, want %d (no duplicates, no omissions)", len(recorded), targets))
	case completed != 1:
		record(name, fmt.Errorf("COMPLETED responses = %d, want exactly 1", completed))
	case inProgress != targets-1:
		record(name, fmt.Errorf("IN_PROGRESS responses = %d, want %d", inProgress, targets-1))
	default:
		pass(fmt.Sprintf("%s: %d targets, %d accepts, exactly one COMPLETED", name, targets, accepted))
	}
}

// checkConcurrentSameIdRace: identical concurrent writes (same id and node)
// must have exactly one winner; every loser gets 409.
func checkConcurrentSameIdRace() {
	name := "concurrent same sample_id/node: one winner"
	view, status := mustCreate(name, createInvestigationReq{
		N: 2, Edges: []investigationEdge{{0, 1}}, Sources: []int64{0},
	})
	if status != http.StatusCreated {
		return
	}
	payload := marshalSample(addSampleReq{SampleID: sid("same-id"), Node: 1, Result: "clean"})

	const racers = 16
	codes := make([]int, racers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			st, _, _, err := postPath(http.MethodPost, samplePath(view.ID), 30*time.Second, payload)
			if err != nil {
				codes[i] = 0
				return
			}
			codes[i] = st
		}(i)
	}
	close(start)
	wg.Wait()

	winners, conflicts := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			winners++
		case http.StatusConflict:
			conflicts++
		}
	}
	if winners != 1 || conflicts != racers-1 {
		record(name, fmt.Errorf("winners = %d (want 1), conflicts = %d (want %d), codes=%v",
			winners, conflicts, racers-1, codes))
		return
	}
	pass(fmt.Sprintf("%s: 1x201 + %dx409", name, racers-1))
}

func samplePath(id string) string {
	return "/investigations/" + id + "/samples"
}

func marshalSample(req addSampleReq) []byte {
	b, _ := json.Marshal(req)
	return b
}

// mustCreate creates an investigation, records a failure on transport or
// non-2xx-unexpected status and returns the decoded view with its status.
func mustCreate(name string, req createInvestigationReq) (investigationDoc, int) {
	body, err := json.Marshal(req)
	if err != nil {
		record(name, fmt.Errorf("marshal: %w", err))
		return investigationDoc{}, 0
	}
	st, doc, _, err := postPath(http.MethodPost, "/investigations", 10*time.Second, body)
	if err != nil {
		record(name, err)
		return investigationDoc{}, 0
	}
	if st != http.StatusCreated {
		record(name, fmt.Errorf("create status = %d, want 201 (body %v)", st, doc))
		return investigationDoc{}, st
	}
	view, err := decodeInvestigation(doc)
	if err != nil {
		record(name, err)
		return investigationDoc{}, st
	}
	return view, st
}

func mustAddSample(name, id string, req addSampleReq) (investigationDoc, int) {
	body, err := json.Marshal(req)
	if err != nil {
		record(name, fmt.Errorf("marshal: %w", err))
		return investigationDoc{}, 0
	}
	st, doc, _, err := postPath(http.MethodPost, samplePath(id), 10*time.Second, body)
	if err != nil {
		record(name, err)
		return investigationDoc{}, 0
	}
	if st != http.StatusCreated {
		if envelopeShape(doc) == "" {
			record(name, fmt.Errorf("sample %+v status = %d without error envelope", req, st))
		} else {
			record(name, fmt.Errorf("sample %+v status = %d, want 201 (body %v)", req, st, doc))
		}
		return investigationDoc{}, st
	}
	view, err := decodeInvestigation(doc)
	if err != nil {
		record(name, err)
		return investigationDoc{}, st
	}
	return view, st
}

func decodeInvestigation(doc map[string]json.RawMessage) (investigationDoc, error) {
	raw, ok := doc["id"]
	if !ok {
		return investigationDoc{}, fmt.Errorf("response missing complete investigation view: %v", doc)
	}
	var view investigationDoc
	b, err := json.Marshal(doc)
	if err != nil {
		return investigationDoc{}, err
	}
	if err := json.Unmarshal(b, &view); err != nil {
		return investigationDoc{}, fmt.Errorf("decode investigation view: %w (%s)", err, raw)
	}
	return view, nil
}

func assertTargets(name string, view investigationDoc, want []int) {
	if len(view.Targets) != len(want) {
		record(name, fmt.Errorf("targets = %v, want %v", view.Targets, want))
		return
	}
	for i, t := range view.Targets {
		if t != want[i] {
			record(name, fmt.Errorf("targets = %v, want %v", view.Targets, want))
			return
		}
	}
}
