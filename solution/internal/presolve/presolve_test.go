package presolve

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"tasr/internal/ecmp"
	"tasr/internal/eval"
	"tasr/internal/graph"
	"tasr/internal/io"
	"tasr/internal/model"
	"tasr/internal/snap"
)

// boundsTest: pure bookkeeping, no instance data.
func TestBoundsKeepTightestInterval(t *testing.T) {
	b := NewBounds()
	b.PinLower(3, 7, 12.5)
	if lb, ok := b.Lower(3, 7); !ok || lb != 12.5 {
		t.Fatalf("Lower = %v, %v; want 12.5, true", lb, ok)
	}
	if ub, ok := b.Upper(3, 7); !ok || !math.IsInf(ub, 1) {
		t.Fatalf("Upper = %v, %v; want +Inf, true", ub, ok)
	}
	// A later proof of immovability tightens the same cell: the interval becomes
	// the tighter one and the provenance survives.
	b.PinProven(3, 7, 12.5, 12.5, "sink-dom")
	bd, _ := b.Get(3, 7)
	if !bd.Proven || bd.LB != 12.5 || bd.UB != 12.5 || bd.Source != "sink-dom" {
		t.Fatalf("after PinProven: %+v", bd)
	}
	keys := b.ProvenKeys()
	if len(keys) != 1 || keys[0].T != 3 || keys[0].A != 7 {
		t.Fatalf("ProvenKeys = %v", keys)
	}
	// A weaker bound must not widen the interval.
	b.PinProven(3, 7, 1, 99, "weaker")
	bd, _ = b.Get(3, 7)
	if bd.LB != 12.5 || bd.UB != 12.5 {
		t.Fatalf("weaker bound widened the interval: %+v", bd)
	}
	// FloorMap carries the LB of every recorded cell, Proven or not.
	b.PinLower(0, 1, 4)
	fm := b.FloorMap()
	if fm[Key{T: 0, A: 1}] != 4 || fm[Key{T: 3, A: 7}] != 12.5 || len(fm) != 2 {
		t.Fatalf("FloorMap = %v", fm)
	}
	// SatFloor is the max saturation over the recorded floors; a cell whose cap
	// index is out of range (or non-positive) is ignored.
	cap := []float64{10, 8, 0, 0, 0, 0, 0, 5}
	if got := b.SatFloor(cap); math.Abs(got-12.5/5) > 1e-12 {
		t.Fatalf("SatFloor = %v, want %v", got, 12.5/5)
	}
}

func TestParseMode(t *testing.T) {
	for _, s := range []string{"", "unmovable", "on"} {
		if m, err := ParseMode(s); err != nil || m != Unmovable {
			t.Fatalf("ParseMode(%q) = %v, %v", s, m, err)
		}
	}
	for _, s := range []string{"off", "none"} {
		if m, err := ParseMode(s); err != nil || m != Off {
			t.Fatalf("ParseMode(%q) = %v, %v", s, m, err)
		}
	}
	if _, err := ParseMode("bogus"); err == nil {
		t.Fatal("ParseMode(bogus) accepted")
	}
}

// loadInstance builds the graph and empty snapshot of a setA/setB instance, or
// skips the test when the challenge data is not on disk.
func loadInstance(t *testing.T, name string) (*model.Instance, *graph.Graph, *snap.Snap) {
	t.Helper()
	root := filepath.Join("..", "..", "..")
	inst, err := io.LoadInstance(filepath.Join(root, "setA", name))
	if err != nil {
		inst, err = io.LoadInstance(filepath.Join(root, "setB", name))
	}
	if err != nil {
		t.Skipf("%s not available under %s: %v", name, root, err)
	}
	g := graph.New(inst)
	sn, err := snap.NewEmpty(inst, g, ecmp.NewCache(g, inst.Scenario.Blocked, 0))
	if err != nil {
		t.Fatal(err)
	}
	return inst, g, sn
}

// TestSetA01FloorIsTheSprintFirstBit checks the claim the whole feature rests
// on for setA-01: arc 29 at slot 0 is the unique tight incoming arc of node 14,
// so the three demands targeting 14 must cross it in every routing.  Their sum
// is a load floor, and truncating its saturation to 6 decimals gives exactly the
// first bit of the sprint reference vector (929383) -- the reference solution's
// first bit is therefore *provably optimal*.
func TestSetA01FloorIsTheSprintFirstBit(t *testing.T) {
	inst, g, sn := loadInstance(t, "setA-01")
	opts := Options{Mode: Unmovable, TimeLimit: 20 * time.Second}
	b := NewBounds()
	rep, err := Run(inst, g, sn, b, opts)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Revoked != 0 {
		t.Errorf("revoked = %d: a forced sum exceeded the observed load, so a criterion is unsound", rep.Revoked)
	}
	// Every recorded floor must be positive and at most the observed load: the
	// floor is a lower bound, the load of a real routing sits at or above it.
	load := sn.Load()
	T := inst.NSlots
	fm := b.FloorMap()
	if len(fm) == 0 {
		t.Fatal("no floor recorded on setA-01")
	}
	for k, lb := range fm {
		cur := load[k.A*T+k.T]
		if lb <= 0 || cur < lb-1e-9*math.Max(1, lb) {
			t.Errorf("floor %v = %.6f exceeds observed load %.6f", k, lb, cur)
		}
	}
	key := Key{T: 0, A: 29}
	lb, ok := b.Lower(key.T, key.A)
	if !ok {
		t.Fatalf("no floor on t=0 arc=%d(7->14); floors=%v", key.A, keysOf(fm))
	}
	if want := 519.5256; math.Abs(lb-want) > 5e-3 {
		t.Errorf("floor(0,29) = %.6f, want ~%.4f", lb, want)
	}
	bd, _ := b.Get(key.T, key.A)
	if got := eval.RankInt(bd.LB / g.Cap[key.A]); got != 929383 {
		t.Errorf("first-bit rank of floor(0,29) = %d, want 929383", got)
	}
	if got := eval.RankInt(rep.LBSat); got != 929383 {
		t.Errorf("report LBSat rank = %d (raw %.9f), want 929383", got, rep.LBSat)
	}
	if bd.Source == "" {
		t.Error("floor on (0,29) has no provenance tag")
	}
	// The snapshot is an actual routing, so the max saturation can never be
	// below the reported global floor.
	if maxSat := eval.SortedDesc(sn.Saturations())[0]; maxSat < rep.LBSat-1e-9 {
		t.Errorf("observed max saturation %.9f below the claimed floor %.9f", maxSat, rep.LBSat)
	}
}

// TestStructuralFloorsAreSubsetsOfTheExactTest cross-checks the cheap criteria
// (classes 1 and 2) against the exact class-3 dodge test, which is the reference
// for "must this demand cross the arc?".  Two invariants per positive-load cell:
//
//	forced_exact <= observed load   the exact set is a real subset of the
//	                                traffic, so its volume cannot exceed it
//	forced_cheap <= forced_exact    the structural criteria only over-claim
//	                                when their bookkeeping is broken (an earlier
//	                                version double-counted a demand that both
//	                                the bridge and the dominance criterion found)
//
// Both hold for every routing, so a violation means the floor is unsound.
func TestStructuralFloorsAreSubsetsOfTheExactTest(t *testing.T) {
	inst, g, sn := loadInstance(t, "setA-01")
	b := NewBounds()
	opts := Options{Mode: Unmovable, TimeLimit: 20 * time.Second}
	if _, err := Run(inst, g, sn, b, opts); err != nil {
		t.Fatal(err)
	}
	j := NewJudge(inst, g, opts)
	load := sn.Load()
	T := inst.NSlots
	tol := 1e-9
	checked, claimed := 0, 0
	for slot := 0; slot < T; slot++ {
		for a := 0; a < g.M; a++ {
			cur := load[a*T+slot]
			if cur <= 0 {
				continue
			}
			vol, _ := j.Exact(slot, a)
			if vol > cur+tol*math.Max(1, cur) {
				t.Errorf("t=%d arc=%d: exact forced volume %.6f exceeds load %.6f", slot, a, vol, cur)
			}
			checked++
			if lb, ok := b.Lower(slot, a); ok {
				claimed++
				if lb > vol+tol*math.Max(1, vol) {
					t.Errorf("t=%d arc=%d: structural floor %.6f exceeds exact forced volume %.6f", slot, a, lb, vol)
				}
			}
		}
	}
	if claimed == 0 {
		t.Fatal("the structural pass claimed no floor at all")
	}
	t.Logf("checked %d positive-load cells, %d with a structural floor", checked, claimed)
}

func keysOf(m map[Key]float64) []Key {
	out := make([]Key, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
