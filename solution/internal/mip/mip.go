package mip

import (
	"fmt"
	"os"
	"sort"

	"tasr/internal/graph"
	"tasr/internal/gurobi"
	"tasr/internal/model"
	"tasr/internal/snap"
)

// Choice is one selected candidate: the waypoints chosen for (d, t).
type Choice struct {
	D   int
	T   int
	Wps []int
}

// Result carries the solver outcome and the chosen waypoints for each pair.
type Result struct {
	Status   int
	ObjVal   float64
	Choices  []Choice
	HasValue bool
}

// SolveOptions configures one MIP solve.
type SolveOptions struct {
	MaxPeel int // reserved: lex peel depth (>=1); peel 1 minimises max saturation.
}

// Problem is the fully precomputed selection model over one slot t.
type Problem struct {
	inst    *model.Instance
	g       *graph.Graph
	snap    *snap.Snap
	t       int
	m       int
	pairs   []Pair
	delta   [][]float64 // per pair: delta[k*m + a]
	tracked []int       // arc positions (slot t) whose load can vary
	caps    []float64   // capacity per tracked arc
	base    []float64   // current load of tracked arcs
}

// Build constructs the MIP problem from a pool.
func Build(p *Pool, sn *snap.Snap) (*Problem, error) {
	m, T := p.m, p.inst.NSlots
	prob := &Problem{inst: p.inst, g: p.g, snap: sn, t: p.t, m: m, pairs: p.Pairs}

	// 1. Tracked arcs: any arc (at slot t) whose load differs between two
	//    candidates of the same pair.
	diff := map[int]bool{}
	for _, pr := range p.Pairs {
		c0 := pr.Cand[0].Load
		for _, c := range pr.Cand[1:] {
			for a := 0; a < m; a++ {
				if c.Load[a] != c0[a] {
					diff[a] = true
				}
			}
		}
	}
	for a := range diff {
		prob.tracked = append(prob.tracked, a)
	}
	sort.Ints(prob.tracked)

	// 2. Delta vectors per pair per candidate and arc base loads.
	cur := p.snap.Load() // arc-major [a*T + t]
	for _, pr := range p.Pairs {
		c0 := pr.Cand[0].Load
		d := make([]float64, len(pr.Cand)*m)
		for k, c := range pr.Cand {
			for a := 0; a < m; a++ {
				d[k*m+a] = c.Load[a] - c0[a]
			}
		}
		prob.delta = append(prob.delta, d)
	}
	for _, a := range prob.tracked {
		prob.caps = append(prob.caps, p.g.Cap[a])
		prob.base = append(prob.base, cur[a*T+p.t])
	}
	return prob, nil
}

// Solve runs the lexicographic peeling MIP.  Layer 1 picks exactly one
// candidate per pair so that the global maximum saturation (first bit) over
// the tracked arcs is minimised (one shared continuous z).  Each later layer
// locks the arcs that attained the previous optimum at their attained load
// (upper bound only, so they may still drop) and minimises a fresh z over the
// remaining arcs, pushing down the second, third, ... entries of the vector in
// turn.  Per-pair convexity and per-transition Hamming-budget rows are present
// in every layer.
func (pr *Problem) Solve(opts SolveOptions) (*Result, error) {
	if opts.MaxPeel <= 0 {
		opts.MaxPeel = 1
	}
	// lockedLoad[a] caps the load of a tracked arc that was peeled into an
	// earlier layer; its load may only decrease from that point onward.
	lockedLoad := map[int]float64{}
	var best *Result
	for layer := 0; layer < opts.MaxPeel; layer++ {
		// Layer 1 models the true global first bit: z is floored by the max
		// saturation of every immutable (frozen) cell.  If that floor is
		// attained only by frozen arcs (no tracked arc reaches z*), the top
		// tier is untouchable and later layers must be free to push the
		// tracked arcs below it, so their z carries no floor.
		zlb := 0.0
		if layer == 0 {
			zlb = pr.constantFloor()
		}
		res, load, err := pr.solveLayer(lockedLoad, zlb)
		if err != nil {
			return nil, err
		}
		if res == nil || !res.HasValue || len(res.Choices) == 0 {
			return res, nil
		}
		best = res
		if layer == opts.MaxPeel-1 {
			break
		}
		// Peel: every still-active tracked arc whose saturation reaches the
		// current optimum z is pinned at its attained load and excluded from
		// the next layer's max.  A layer whose optimum is reached only by the
		// frozen background locks nothing and the peel simply moves on.
		for i, a := range pr.tracked {
			if _, ok := lockedLoad[a]; ok {
				continue
			}
			if load[i]/pr.caps[i] >= res.ObjVal-1e-7 {
				lockedLoad[a] = load[i]
			}
		}
	}
	return best, nil
}

// solveLayer builds and solves one peel layer of the selection MIP.
// lockedLoad holds load caps for arcs already assigned to earlier layers
// (their row becomes an absolute upper bound, no z); zlb is the lower bound
// of this layer's z (constantFloor for the first layer, 0 afterwards).  It
// returns the result and, per tracked arc, the load attained in the optimum.
func (pr *Problem) solveLayer(lockedLoad map[int]float64, zlb float64) (*Result, []float64, error) {
	inst, m := pr.inst, pr.m

	// Variable indexing: all candidate binaries, then one continuous z.
	nX := 0
	for _, prr := range pr.pairs {
		nX += len(prr.Cand)
	}
	zIdx := nX
	if nX == 0 {
		return nil, nil, fmt.Errorf("mip: no candidate variables")
	}

	env, err := gurobi.EnvNew()
	if err != nil {
		return nil, nil, err
	}
	defer env.Free()
	model, err := env.NewModel("mip")
	if err != nil {
		return nil, nil, err
	}
	defer model.Free()
	if err := model.SetIntAttr("ModelSense", gurobi.ModelSenseMinimize); err != nil {
		return nil, nil, err
	}

	obj := make([]float64, nX+1)
	lb := make([]float64, nX+1)
	ub := make([]float64, nX+1)
	vt := make([]byte, nX+1)
	for i := 0; i < nX; i++ {
		vt[i] = 'B'
		ub[i] = 1
	}
	vt[zIdx] = 'C'
	ub[zIdx] = 1e30
	lb[zIdx] = zlb
	obj[zIdx] = 1 // objective: minimise z (the current layer's max saturation)
	if _, err := model.AddVars(obj, lb, ub, vt); err != nil {
		return nil, nil, err
	}

	// Base variable index of each pair.
	base := make([]int, len(pr.pairs))
	acc := 0
	for i, prr := range pr.pairs {
		base[i] = acc
		acc += len(prr.Cand)
	}

	var cbeg []int32
	var cind []int32
	var cval []float64
	var sense []byte
	var rhs []float64
	addRow := func(cols []int, vals []float64, s byte, r float64) {
		if len(cols) == 0 {
			return // constant row; do not emit
		}
		cbeg = append(cbeg, int32(len(cind)))
		for j, col := range cols {
			cind = append(cind, int32(col))
			cval = append(cval, vals[j])
		}
		sense = append(sense, s)
		rhs = append(rhs, r)
	}

	// Convexity per pair: sum_k x_{d,k} = 1.
	for i, prr := range pr.pairs {
		cols := make([]int, len(prr.Cand))
		vals := make([]float64, len(prr.Cand))
		for k := range prr.Cand {
			cols[k] = base[i] + k
			vals[k] = 1
		}
		addRow(cols, vals, '=', 1)
	}

	// Arc rows.  Current total load on arc a (slot pr.t) is pr.base[a]; each
	// chosen candidate changes it by delta, so the new load is
	//   base[a] + sum(delta * x).
	// Unlocked arc:  base + sum(delta*x) <= cap*z  ->  sum(delta*x) - cap*z <= -base.
	// Locked arc:    base + sum(delta*x) <= lockedLoad  ->  sum(delta*x) <= lockedLoad - base.
	for ri, a := range pr.tracked {
		var cols []int
		var vals []float64
		for i, prr := range pr.pairs {
			d := pr.delta[i]
			for k := range prr.Cand {
				dk := d[k*m+a]
				if dk != 0 {
					cols = append(cols, base[i]+k)
					vals = append(vals, dk)
				}
			}
		}
		if capLoad, ok := lockedLoad[a]; ok {
			addRow(cols, vals, '<', capLoad-pr.base[ri])
		} else {
			cols = append(cols, zIdx)
			vals = append(vals, -pr.caps[ri])
			addRow(cols, vals, '<', -pr.base[ri])
		}
	}

	// Hamming-budget rows for every affected transition.
	for _, tt := range pr.affectedTransitions() {
		if tt <= 0 || tt >= inst.NSlots {
			continue
		}
		budget := -1
		if tt < len(inst.Scenario.Budget) {
			budget = inst.Scenario.Budget[tt]
		}
		if budget < 0 {
			continue // no declared budget for this transition
		}
		curCost := pr.snap.CostAt(tt)
		var cols []int
		var vals []float64
		for i, prr := range pr.pairs {
			dk := pr.transDelta(prr.D, tt)
			for k := range prr.Cand {
				if dk[k] != 0 {
					cols = append(cols, base[i]+k)
					vals = append(vals, float64(dk[k]))
				}
			}
		}
		addRow(cols, vals, '<', float64(budget-curCost))
	}

	// CSR terminator: cbeg must hold ncon+1 entries.
	cbeg = append(cbeg, int32(len(cind)))

	if err := model.AddConstrs(cbeg, cind, cval, sense, rhs); err != nil {
		return nil, nil, err
	}
	if err := model.Update(); err != nil {
		return nil, nil, err
	}
	if lp := os.Getenv("TASR_GRB_LP"); lp != "" {
		if err := model.Write(lp); err != nil {
			return nil, nil, err
		}
	}
	if err := model.Optimize(); err != nil {
		return nil, nil, err
	}
	status, err := model.IntAttr("Status")
	if err != nil {
		return nil, nil, err
	}
	res := &Result{Status: status}
	if status == gurobi.StatusOptimal || status == gurobi.StatusTimeLimit ||
		status == gurobi.StatusSuboptimal || status == gurobi.StatusInterrupted {
		res.HasValue = true
		objVal, err := model.DblAttr("ObjVal")
		if err != nil {
			return nil, nil, err
		}
		res.ObjVal = objVal
		xv := make([]float64, nX)
		if err := model.X(xv); err != nil {
			return nil, nil, err
		}
		for i, prr := range pr.pairs {
			best := 0
			for k := range prr.Cand {
				if xv[base[i]+k] > xv[base[i]+best]+1e-6 {
					best = k
				}
			}
			res.Choices = append(res.Choices, Choice{D: prr.D, T: prr.T, Wps: prr.Cand[best].Wps})
		}
		// Attained load per tracked arc at this optimum.
		load := make([]float64, len(pr.tracked))
		for ri, a := range pr.tracked {
			l := pr.base[ri]
			for i, prr := range pr.pairs {
				d := pr.delta[i]
				for k := range prr.Cand {
					l += d[k*m+a] * xv[base[i]+k]
				}
			}
			load[ri] = l
		}
		return res, load, nil
	}
	return res, nil, nil
}

// transDelta returns, for transition tt, the vector over candidates of the
// Hamming-cost difference vs the current candidate of pair (d, slot t).
func (pr *Problem) transDelta(d, tt int) []int {
	prr := pr.findPair(d)
	if prr == nil {
		return nil
	}
	inst := pr.inst
	n := len(prr.Cand)
	out := make([]int, n)
	var other []int
	// other = the waypoints of the other slot of this transition (unchanged).
	if tt == pr.t {
		// transition (t-1) -> t ; moving slot is t (== tt)
		other = pr.snap.GetWaypoints(d, tt-1)
		for k, c := range prr.Cand {
			out[k] = snap.PathDist(inst, d, other, c.Wps) - snap.PathDist(inst, d, other, prr.Cand[0].Wps)
		}
	} else if tt == pr.t+1 {
		// transition t -> t+1 ; moving slot is t (== tt-1)
		other = pr.snap.GetWaypoints(d, tt)
		for k, c := range prr.Cand {
			out[k] = snap.PathDist(inst, d, c.Wps, other) - snap.PathDist(inst, d, prr.Cand[0].Wps, other)
		}
	}
	return out
}

// findPair returns the pair of demand d at the focus slot, or nil.
func (pr *Problem) findPair(d int) *Pair {
	for i := range pr.pairs {
		if pr.pairs[i].D == d {
			return &pr.pairs[i]
		}
	}
	return nil
}

// affectedTransitions returns transition indices whose cost can change when
// slot pr.t routings change.
func (pr *Problem) affectedTransitions() []int {
	t := pr.t
	var out []int
	if t >= 1 {
		out = append(out, t)
	}
	if t+1 < pr.inst.NSlots {
		out = append(out, t+1)
	}
	return out
}

// constantFloor returns the maximum saturation over all cells that cannot
// change (every (arc, slot) except tracked arcs at the focus slot).
func (pr *Problem) constantFloor() float64 {
	sat := pr.snap.Saturations()
	m, T := pr.inst.NArcs(), pr.inst.NSlots
	tracked := map[int]bool{}
	for _, a := range pr.tracked {
		tracked[a] = true
	}
	floor := 0.0
	for a := 0; a < m; a++ {
		for tt := 0; tt < T; tt++ {
			if tt == pr.t && tracked[a] {
				continue
			}
			if sat[a*T+tt] > floor {
				floor = sat[a*T+tt]
			}
		}
	}
	return floor
}
