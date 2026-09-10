package gurobi

import (
	"math"
	"testing"
)

// TestConstraintDual pins down the one thing the dual probe cannot be built on
// faith: that the element/list accessors really do return constraint duals.
//
// Gurobi 13 exports no GRBgetdblattrconstr, so the binding reaches Pi through
// GRBgetdblattrelement and GRBgetdblattrlist with a constraint index.  That is
// not something to take on trust -- if those accessors were variable-only, the
// probe would silently read garbage (variable 0's reduced cost, say) and every
// conclusion drawn from it would be wrong.
//
// The model is a hand-solvable LP so the expected duals are known:
//
//	min z
//	R0:  x + y      = 1      (the pool's convexity row)
//	R1:  2x + 3y - z <= 0    (the "cell" row: z is the max of 2x + 3y)
//	R2:  x + y      <= 2     (slack: implied by R0, can never bind)
//	x, y in [0,1], z >= 0
//
// Optimum is z = 2 at (x, y) = (1, 0).  R1 is active there, so its price is
// +-1 (increasing the row's RHS by one unit lets z fall by one); R2 is slack by
// construction, so its price must be exactly 0.  Signs are deliberately not
// asserted: the point is that a binding row has a non-zero price and a slack row
// has a zero one, which no variable-attribute mix-up would satisfy.
func TestConstraintDual(t *testing.T) {
	env, err := EnvNew()
	if err != nil {
		t.Skipf("gurobi unavailable: %v", err)
	}
	defer env.Free()

	model, err := env.NewModel("dual")
	if err != nil {
		t.Fatalf("NewModel: %v", err)
	}
	defer model.Free()
	if err := model.SetIntAttr("ModelSense", ModelSenseMinimize); err != nil {
		t.Fatalf("ModelSense: %v", err)
	}

	obj := []float64{0, 0, 1}
	lb := []float64{0, 0, 0}
	ub := []float64{1, 1, 1e30}
	if _, err := model.AddVars(obj, lb, ub, []byte{'C', 'C', 'C'}); err != nil {
		t.Fatalf("AddVars: %v", err)
	}

	cbeg := []int32{0, 2, 5, 7}
	cind := []int32{0, 1, 0, 1, 2, 0, 1}
	cval := []float64{1, 1, 2, 3, -1, 1, 1}
	sense := []byte{'=', '<', '<'}
	rhs := []float64{1, 0, 2}
	if err := model.AddConstrs(cbeg, cind, cval, sense, rhs); err != nil {
		t.Fatalf("AddConstrs: %v", err)
	}
	if err := model.Update(); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := model.Optimize(); err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	st, err := model.IntAttr("Status")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st != StatusOptimal {
		t.Fatalf("status = %d, want optimal (%d)", st, StatusOptimal)
	}
	ncon, err := model.IntAttr("NumConstrs")
	if err != nil {
		t.Fatalf("NumConstrs: %v", err)
	}
	if ncon != 3 {
		t.Fatalf("NumConstrs = %d, want 3", ncon)
	}

	// z* = 2: the cell row really does pin the objective.
	zval, err := model.DblAttr("ObjVal")
	if err != nil {
		t.Fatalf("ObjVal: %v", err)
	}
	if math.Abs(zval-2) > 1e-9 {
		t.Fatalf("ObjVal = %v, want 2", zval)
	}

	// R1 (active) must have a non-zero price of magnitude 1; R2 (slack) 0.
	pi1, err := model.DblAttrElement("Pi", 1)
	if err != nil {
		t.Fatalf("Pi on constraint 1: %v", err)
	}
	if math.Abs(math.Abs(pi1)-1) > 1e-9 {
		t.Fatalf("Pi(constraint 1) = %v, want +-1 (an active row with a unit price)", pi1)
	}
	pi2, err := model.DblAttrElement("Pi", 2)
	if err != nil {
		t.Fatalf("Pi on constraint 2: %v", err)
	}
	if math.Abs(pi2) > 1e-9 {
		t.Fatalf("Pi(constraint 2) = %v, want 0 (the row is slack by construction)", pi2)
	}

	// The list accessor is the batch form the probe uses; it must agree.
	ind := []int32{0, 1, 2}
	vals := make([]float64, 3)
	if err := model.GetDblAttrList("Pi", ind, vals); err != nil {
		t.Fatalf("GetDblAttrList: %v", err)
	}
	if math.Abs(vals[1]-pi1) > 1e-12 || math.Abs(vals[2]-pi2) > 1e-12 {
		t.Fatalf("GetDblAttrList = %v, want index 1 = %v and index 2 = %v", vals, pi1, pi2)
	}
}
