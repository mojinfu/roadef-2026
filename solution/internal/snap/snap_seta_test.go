package snap

import (
	"os"
	"path/filepath"
	"testing"

	"tasr/internal/ecmp"
	"tasr/internal/eval"
	"tasr/internal/graph"
	"tasr/internal/io"
)

// TestSnapshotPatchBatteryEqualsFullEvalOnSetA is an integration check on the
// real setA-01 instance: build a snapshot from the empty solution, apply the
// waypoint batches of every _pyref/battery solution via incremental Patch, then
// compare both the resulting saturations and total Hamming cost against a full
// evaluation of the same solution.  Skips silently when the data files are not
// present (unit-test environment).
func TestSnapshotPatchBatteryEqualsFullEvalOnSetA(t *testing.T) {
	inst, err := io.LoadInstance(instancePrefix())
	if err != nil {
		t.Skipf("setA data not available: %v", err)
	}
	g := graph.New(inst)
	cache := ecmp.NewCache(g, inst.Scenario.Blocked, 0)
	ev := eval.NewEvaluator(inst, g, cache)

	batteryDir := filepath.Join("..", "..", "_pyref", "battery")
	entries, err := os.ReadDir(batteryDir)
	if err != nil {
		t.Skipf("battery dir not available: %v", err)
	}
	checked := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		path := filepath.Join(batteryDir, e.Name())
		sol, err := io.ReadSolution(path, inst)
		if err != nil {
			t.Fatalf("%s: read: %v", e.Name(), err)
		}
		snap0, err := NewEmpty(inst, g, cache)
		if err != nil {
			t.Fatalf("%s: new snapshot: %v", e.Name(), err)
		}
		for sl := 0; sl < inst.NSlots; sl++ {
			changes := map[int][]int{}
			for d := 0; d < inst.NDemands(); d++ {
				if len(sol.Waypoints[d][sl]) > 0 {
					changes[d] = sol.Waypoints[d][sl]
				}
			}
			if len(changes) > 0 {
				if err := snap0.Patch(sl, changes); err != nil {
					t.Fatalf("%s: patch t=%d: %v", e.Name(), sl, err)
				}
			}
		}
		if err := snap0.Rebuild(); err != nil {
			t.Fatalf("%s: rebuild: %v", e.Name(), err)
		}
		fullSat, err := ev.Saturations(sol)
		if err != nil {
			t.Fatalf("%s: full eval: %v", e.Name(), err)
		}
		gotSat := snap0.Saturations()
		if !satCloseEnough(gotSat, fullSat, 1e-8) {
			t.Fatalf("%s: incremental saturation != full eval", e.Name())
		}
		if got := snap0.TotalCost(); got != SolutionTotalCost(inst, sol) {
			t.Fatalf("%s: snapshot cost=%d != solution cost=%d", e.Name(), got, SolutionTotalCost(inst, sol))
		}
		checked++
	}
	if checked == 0 {
		t.Skip("no battery json files")
	}
	t.Logf("checked %d battery solutions on setA-01", checked)
}

func satCloseEnough(a, b []float64, tol float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		d := a[i] - b[i]
		if d < 0 {
			d = -d
		}
		if d > tol {
			return false
		}
	}
	return true
}

func instancePrefix() string {
	return filepath.Join("..", "..", "..", "setA", "setA-01")
}
