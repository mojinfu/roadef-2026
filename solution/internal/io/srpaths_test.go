package io

import (
	"bytes"
	"encoding/json"
	"testing"

	"tasr/internal/model"
)

// smallInstance builds an instance whose network-file node ids are NOT equal to
// their positions (ids listed in arbitrary order, as several setA files do).
func smallInstance() *model.Instance {
	return &model.Instance{
		Name:      "tiny",
		NodeNames: []string{"a", "b", "c"},
		NodeIDs:   []int{30, 10, 20}, // position 0 -> id 30, 1 -> id 10, 2 -> id 20
		Arcs: []model.Arc{
			{ID: 0, From: 0, To: 1, Metric: 1, Capacity: 10},
			{ID: 1, From: 1, To: 2, Metric: 1, Capacity: 10},
			{ID: 2, From: 0, To: 2, Metric: 5, Capacity: 10},
		},
		NSlots:  1,
		Demands: []model.Demand{{Source: 0, Target: 2, Volume: []float64{1}}},
		Scenario: model.Scenario{
			MaxSegments: 4,
			Budget:      []int{0},
			Blocked:     [][]bool{{false, false, false}},
		},
	}
}

func TestWriteReadRoundTripMapsNodeIDs(t *testing.T) {
	inst := smallInstance()
	sol := model.EmptySolution(1, 1)
	sol.Set(0, 0, []int{1}) // waypoint = position of node id 10

	doc := SolutionToDict(sol, inst)
	if len(doc.Srpaths) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(doc.Srpaths))
	}
	e := doc.Srpaths[0]
	if e.D != 0 || e.T != 0 || len(e.W) != 1 || e.W[0] != 10 {
		t.Fatalf("entry = %+v, want d=0 t=0 w=[10] (network id)", e)
	}

	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	back, err := LoadSolutionFromBytes(data, inst)
	if err != nil {
		t.Fatal(err)
	}
	got := back.Waypoints[0][0]
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("round-trip waypoint = %v, want [1] (position)", got)
	}
}

func TestEmptyWaypointsOmitted(t *testing.T) {
	inst := smallInstance()
	sol := model.EmptySolution(1, 1) // no waypoints at all
	doc := SolutionToDict(sol, inst)
	if len(doc.Srpaths) != 0 {
		t.Fatalf("empty solution should serialize to no entries, got %d", len(doc.Srpaths))
	}
}

func TestReaderAcceptsOmittedAndFirstLetterKeys(t *testing.T) {
	inst := smallInstance()
	// Keys "w" before "d"/"t", order shuffled; and an omitted d/t pair must
	// remain empty (== direct path).
	raw := []byte(`{"srpaths":[{"w":[10],"d":0,"t":0}]}`)
	sol, err := LoadSolutionFromBytes(raw, inst)
	if err != nil {
		t.Fatal(err)
	}
	if len(sol.Waypoints[0][0]) != 1 || sol.Waypoints[0][0][0] != 1 {
		t.Fatalf("waypoint = %v, want position 1", sol.Waypoints[0][0])
	}
}

func TestCompactJSONMatchesReferenceFormat(t *testing.T) {
	inst := smallInstance()
	sol := model.EmptySolution(1, 1)
	sol.Set(0, 0, []int{1})
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(SolutionToDict(sol, inst)); err != nil {
		t.Fatal(err)
	}
	// Reference python writer output for the same structure:
	// {"srpaths":[{"d":0,"t":0,"w":[10]}]}
	got := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	want := `{"srpaths":[{"d":0,"t":0,"w":[10]}]}`
	if string(got) != want {
		t.Fatalf("json = %s, want %s", got, want)
	}
}
