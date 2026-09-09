package snap

import (
	"math"
	"testing"

	"tasr/internal/ecmp"
	"tasr/internal/eval"
	"tasr/internal/graph"
	"tasr/internal/model"
)

// tinyInstance: 4 nodes, arcs 0:0->1,1:0->2,2:1->3,3:2->3,4:1->2 (m=100);
// two slots; two demands.  Matches the ecmp test graph.
func tinyInstance() *model.Instance {
	return &model.Instance{
		Name:      "tiny",
		NodeNames: []string{"a", "b", "c", "d"},
		NodeIDs:   []int{0, 1, 2, 3},
		Arcs: []model.Arc{
			{ID: 0, From: 0, To: 1, Metric: 1, Capacity: 100},
			{ID: 1, From: 0, To: 2, Metric: 1, Capacity: 100},
			{ID: 2, From: 1, To: 3, Metric: 1, Capacity: 100},
			{ID: 3, From: 2, To: 3, Metric: 1, Capacity: 100},
			{ID: 4, From: 1, To: 2, Metric: 100, Capacity: 100},
		},
		NSlots: 2,
		Demands: []model.Demand{
			{Source: 0, Target: 3, Volume: []float64{1.0, 2.0}},
			{Source: 1, Target: 3, Volume: []float64{1.0, 1.0}},
		},
		Scenario: model.Scenario{
			MaxSegments: 6,
			Budget:      []int{0, 100},
			Blocked:     [][]bool{{false, false, false, false, false}, {false, false, false, false, false}},
		},
	}
}

func closeEnough(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if math.Abs(a[i]-b[i]) > 1e-9 {
			return false
		}
	}
	return true
}

func TestPathDistSemantics(t *testing.T) {
	inst := tinyInstance()
	d0 := 0 // s=0 t=3
	cases := []struct {
		a, b []int
		want int
	}{
		{nil, nil, 0},
		{nil, []int{1}, 3},    // empty vs 1 waypoint -> node count 3
		{nil, []int{1, 2}, 4}, // empty vs 2 waypoints -> node count 4
		{[]int{1}, nil, 3},
		{[]int{1}, []int{1}, 0},    // identical
		{[]int{1}, []int{2}, 4},    // swap waypoint -> 4 pairs differ
		{[]int{1, 2}, []int{1}, 3}, // {(0,1)} shared; A-only 2 + B-only 1
		{[]int{1}, []int{1, 2}, 3}, // A {(0,1),(1,3)}; B {(0,1),(1,2),(2,3)}
	}
	for _, c := range cases {
		if got := PathDist(inst, d0, c.a, c.b); got != c.want {
			t.Errorf("PathDist(d0, %v, %v) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestSnapshotPatchEqualsFullEval(t *testing.T) {
	inst := tinyInstance()
	g := graph.New(inst)
	cache := ecmp.NewCache(g, inst.Scenario.Blocked, 0)

	// Direct ECMP: d0 from 0 to 3 splits 0.5 over {0->1,1->3} and 0.5 over
	// {0->2,2->3}; with arc 4 never tight.
	ev := eval.NewEvaluator(inst, g, cache)
	solEmpty := model.EmptySolution(inst.NDemands(), inst.NSlots)
	satFull, err := ev.Saturations(solEmpty)
	if err != nil {
		t.Fatal(err)
	}

	snap0, err := NewEmpty(inst, g, cache)
	if err != nil {
		t.Fatal(err)
	}
	if !closeEnough(snap0.Saturations(), satFull) {
		t.Fatalf("initial snapshot != full eval saturations")
	}

	// Patch demand 0 slot 0 to go via node 1 (waypoint 1): unit route then
	// uses only arc0 and arc2.
	if _, err := snap0.SetWaypoints(0, 0, []int{1}); err != nil {
		t.Fatal(err)
	}
	sol1 := model.EmptySolution(inst.NDemands(), inst.NSlots)
	sol1.Set(0, 0, []int{1})
	satFull1, err := ev.Saturations(sol1)
	if err != nil {
		t.Fatal(err)
	}
	if !closeEnough(snap0.Saturations(), satFull1) {
		t.Fatalf("patched snapshot != full eval saturations")
	}

	// Patch back to empty -> must match the empty solution again.
	if _, err := snap0.SetWaypoints(0, 0, nil); err != nil {
		t.Fatal(err)
	}
	if !closeEnough(snap0.Saturations(), satFull) {
		t.Fatalf("rollback snapshot != full eval saturations")
	}
}

func TestWaypointsTotalCost(t *testing.T) {
	inst := tinyInstance()
	sol := model.EmptySolution(2, 2)
	// Empty solution: all demands direct at both slots -> cost 0.
	if got := SolutionTotalCost(inst, sol); got != 0 {
		t.Fatalf("empty total_cost = %d, want 0", got)
	}
	// Demand 0 waypoint [1] at t=0 only: transition 0->1 costs node count 3.
	sol.Set(0, 0, []int{1})
	if got := SolutionTotalCost(inst, sol); got != 3 {
		t.Fatalf("total_cost = %d, want 3", got)
	}
	// Same waypoint also at t=1: identical paths, cost back to 0.
	sol.Set(0, 1, []int{1})
	if got := SolutionTotalCost(inst, sol); got != 0 {
		t.Fatalf("total_cost = %d, want 0", got)
	}
	// Demand 1 waypoint [2] at t=1 only (source 1 target 3): cost 3.
	sol.Set(1, 1, []int{2})
	if got := SolutionTotalCost(inst, sol); got != 3 {
		t.Fatalf("total_cost = %d, want 3", got)
	}
}
