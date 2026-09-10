package local

import (
	"math"
	"path/filepath"
	"testing"

	"tasr/internal/ecmp"
	"tasr/internal/eval"
	"tasr/internal/graph"
	"tasr/internal/io"
	"tasr/internal/model"
	"tasr/internal/snap"
)

// detourInstance is the smallest network in which the pass has something to do.
//
// Node 0 is the origin and 3 the destination.  The shortest path is 0->1->3,
// metric 2, unique; 0->2->3 costs 4 and is therefore never taken on its own.
// The two arcs of the short path each have capacity 1 and the demand carries
// volume 1 in both slots, so the empty routing saturates them exactly.  The only
// way to move any of that load is a waypoint at node 2, which no shortest path
// would ever choose -- which is precisely what the banned-arc discovery is for.
func detourInstance() *model.Instance {
	return &model.Instance{
		Name:      "detour",
		NodeNames: []string{"0", "1", "2", "3"},
		NodeIDs:   []int{0, 1, 2, 3},
		Arcs: []model.Arc{
			{ID: 0, From: 0, To: 1, Metric: 1, Capacity: 1},
			{ID: 1, From: 1, To: 3, Metric: 1, Capacity: 1},
			{ID: 2, From: 0, To: 2, Metric: 2, Capacity: 4},
			{ID: 3, From: 2, To: 3, Metric: 2, Capacity: 4},
		},
		NSlots:  2,
		Demands: []model.Demand{{Source: 0, Target: 3, Volume: []float64{1, 1}}},
		Scenario: model.Scenario{
			MaxSegments: 2,
			Budget:      []int{0, 10},
			Blocked:     [][]bool{make([]bool, 4), make([]bool, 4)},
		},
	}
}

func newSnap(t *testing.T, inst *model.Instance) (*snap.Snap, *ecmp.Cache) {
	t.Helper()
	g := graph.New(inst)
	cache := ecmp.NewCache(g, inst.Scenario.Blocked, 0)
	sn, err := snap.NewEmpty(inst, g, cache)
	if err != nil {
		t.Fatalf("NewEmpty: %v", err)
	}
	return sn, cache
}

// TestSearchForcedDetourImprovesAndKeepsBudget drives the whole loop end to end
// on the fixture above: two moves (one per slot), each of which has to be
// discovered through the banned-arc detour, and neither of which may leave the
// snapshot over budget or worse in the lexicographic order.
func TestSearchForcedDetourImprovesAndKeepsBudget(t *testing.T) {
	inst := detourInstance()
	sn, cache := newSnap(t, inst)
	g := graph.New(inst)
	start := eval.SortedDesc(sn.Saturations())

	s := New(inst, g, cache.Index(), Options{Rounds: 3})
	res := s.Run(sn)

	if res.Improved != 2 {
		t.Fatalf("improved = %d, want 2 (one detour per slot); skip=%v", res.Improved, res.Skip)
	}
	// The third round finds the attack surface drained onto the detour arcs,
	// which cannot be moved any further: that is what ends the call.
	if res.Rounds != 3 {
		t.Errorf("rounds = %d, want 3 (two productive rounds then an empty one)", res.Rounds)
	}
	if got := sn.GetWaypoints(0, 0); len(got) != 1 || got[0] != 2 {
		t.Errorf("slot 0 waypoints = %v, want [2]", got)
	}
	if got := sn.GetWaypoints(0, 1); len(got) != 1 || got[0] != 2 {
		t.Errorf("slot 1 waypoints = %v, want [2]", got)
	}
	if !sn.BudgetOK() {
		t.Errorf("the result violates the Hamming budget: cost at t=1 is %d, budget %d",
			sn.CostAt(1), inst.Scenario.Budget[1])
	}
	// Volume routed over a capacity-4 detour instead of two capacity-1 arcs:
	// the max saturation drops from 1.0 to 0.25.
	final := eval.SortedDesc(sn.Saturations())
	if eval.LexCompare(final, start) >= 0 {
		t.Errorf("the vector did not improve: max %.6f -> %.6f", start[0], final[0])
	}
	if math.Abs(final[0]-0.25) > 1e-9 {
		t.Errorf("final max = %.9f, want 0.25", final[0])
	}
}

// TestSearchWithoutWaypointBudgetIsNoOp covers the scenario that forbids
// waypoints entirely: nothing can be proposed, so nothing may change.
func TestSearchWithoutWaypointBudgetIsNoOp(t *testing.T) {
	inst := detourInstance()
	inst.Scenario.MaxSegments = 1
	sn, cache := newSnap(t, inst)
	g := graph.New(inst)
	start := eval.SortedDesc(sn.Saturations())

	res := New(inst, g, cache.Index(), Options{}).Run(sn)

	if res.Improved != 0 || res.Skip["no-waypoint-budget"] != 1 {
		t.Fatalf("improved=%d skip=%v, want no move and a no-waypoint-budget skip", res.Improved, res.Skip)
	}
	if len(sn.GetWaypoints(0, 0)) != 0 || len(sn.GetWaypoints(0, 1)) != 0 {
		t.Errorf("a scenario with no waypoint budget was modified")
	}
	if eval.LexCompare(eval.SortedDesc(sn.Saturations()), start) != 0 {
		t.Errorf("the vector changed despite no candidate being available")
	}
}

// TestSearchWithNoLoadIsNoOp covers the attack surface filter: with every
// volume at zero there is no load to rank, so the round has nothing to attack.
func TestSearchWithNoLoadIsNoOp(t *testing.T) {
	inst := detourInstance()
	for i := range inst.Demands {
		inst.Demands[i].Volume = []float64{0, 0}
	}
	sn, cache := newSnap(t, inst)
	g := graph.New(inst)

	res := New(inst, g, cache.Index(), Options{}).Run(sn)

	if res.Improved != 0 || res.Skip["no-surface"] != 1 {
		t.Fatalf("improved=%d skip=%v, want no move and a no-surface skip", res.Improved, res.Skip)
	}
}

// TestSearchIsDeterministic pins the property the solver depends on: same
// instance, same starting snapshot, same waypoints out.  The pass has no
// randomness of its own, and the two map iterations in it (the surface's slot
// set and the arc sets derived from it) are collected into sorted slices first.
func TestSearchIsDeterministic(t *testing.T) {
	inst := detourInstance()
	g := graph.New(inst)

	var first [][]int
	for run := 0; run < 2; run++ {
		sn, cache := newSnap(t, inst)
		New(inst, g, cache.Index(), Options{}).Run(sn)
		var got [][]int
		for sl := 0; sl < inst.NSlots; sl++ {
			got = append(got, append([]int(nil), sn.GetWaypoints(0, sl)...))
		}
		if run == 0 {
			first = got
			continue
		}
		for sl := range got {
			if len(got[sl]) != len(first[sl]) {
				t.Fatalf("run 2 slot %d waypoints = %v, run 1 = %v", sl, got[sl], first[sl])
			}
			for i := range got[sl] {
				if got[sl][i] != first[sl][i] {
					t.Fatalf("run 2 slot %d waypoints = %v, run 1 = %v", sl, got[sl], first[sl])
				}
			}
		}
	}
}

// TestSearchOnSetA01StaysLegal is the integration check on real data, run
// without the MIP loop: repeated calls on the empty solution must never worsen
// the lexicographic vector, must keep every transition inside its budget, and
// must keep every waypoint list inside the instance's max_segments.  Skips
// silently when the data files are absent.
func TestSearchOnSetA01StaysLegal(t *testing.T) {
	inst, err := io.LoadInstance(instancePrefix())
	if err != nil {
		t.Skipf("setA data not available: %v", err)
	}
	sn, cache := newSnap(t, inst)
	g := graph.New(inst)

	s := New(inst, g, cache.Index(), Options{Rounds: 3, CallBudget: 2e9})
	prev := eval.SortedDesc(sn.Saturations())
	for call := 0; call < 3; call++ {
		s.Run(sn)
		cur := eval.SortedDesc(sn.Saturations())
		if eval.LexCompare(cur, prev) > 0 {
			t.Fatalf("call %d worsened the vector (max %.6f -> %.6f)", call, prev[0], cur[0])
		}
		prev = cur
		if !sn.BudgetOK() {
			t.Fatalf("call %d left the solution over budget", call)
		}
		maxWp := inst.Scenario.MaxSegments - 1
		for d := 0; d < inst.NDemands(); d++ {
			for sl := 0; sl < inst.NSlots; sl++ {
				if n := len(sn.GetWaypoints(d, sl)); n > maxWp {
					t.Fatalf("call %d left demand %d slot %d with %d waypoints (max %d)", call, d, sl, n, maxWp)
				}
			}
		}
	}
}

func instancePrefix() string {
	return filepath.Join("..", "..", "..", "setA", "setA-01")
}
