package mip

import (
	"fmt"
	"math"
	"os"
	"sort"
	"time"

	"tasr/internal/graph"
	"tasr/internal/gurobi"
	"tasr/internal/model"
	"tasr/internal/snap"
)

// SlotWps is one slot to overwrite with a waypoint list when a choice is
// materialised.
type SlotWps struct {
	Slot int
	Wps  []int
}

// Choice is one selected candidate: the atomic moves of one demand.
type Choice struct {
	D     int
	Apply []SlotWps // empty for the no-change candidate
}

// Result carries the solver outcome and the chosen candidates.
type Result struct {
	Status   int
	ObjVal   float64
	Choices  []Choice
	HasValue bool
	// Runtime is the wall time Gurobi itself spent across this round's peel
	// layers, in seconds (the model's own Runtime attribute, so it excludes
	// model build and the Go-side bookkeeping).  It is reported so the round
	// loop can show what share of a round is Gurobi and what share is pool
	// building -- a round that spends 2s on candidates and 5ms in Gurobi is a
	// candidate-generation problem, not a MIP problem.
	Runtime float64
}

// Proven reports whether the solve finished to proven optimality, i.e. the last
// peel layer's Gurobi status was Optimal rather than a time limit.  It is the
// gate the outer loop's confidence memory hangs on (design doc §21): a round
// that timed out is not evidence that its seed is immovable, so it must feed
// the not-proven counter instead of the confidence.  Status carries the *last*
// peel layer's status, which is what the doc's "最优仍没打下 first bit" means --
// the final layer proved the tail it was given.
func (r *Result) Proven() bool { return r.Status == gurobi.StatusOptimal }

// SolveOptions configures one MIP solve.
type SolveOptions struct {
	MaxPeel   int           // lex peel depth (>=1); peel 1 minimises max saturation.
	TimeLimit time.Duration // per peel-layer Gurobi cap (0 = solve to completion).
}

// cell is one tracked (slot, arc) saturation cell.
type cell struct {
	slot int
	a    int
}

// Problem is the fully precomputed selection model over one pool universe.
type Problem struct {
	inst    *model.Instance
	g       *graph.Graph
	snap    *snap.Snap
	m       int
	T       int
	slots   []int
	pos     map[int]int // slot -> position in slots
	pairs   []Pair
	tracked []cell // cells whose load can vary
	caps    []float64
	base    []float64       // current load of each tracked cell
	delta   [][]float64     // per pair: delta[k*nCell + ci]
	trans   [][]map[int]int // per pair per candidate: transition -> Hamming delta
	// first-half movable-budget split (design doc §11 budget.py): the frozen
	// demands (this round's waypoints fixed on a transition) keep occupying
	// budget, so the MIP's movable budget on transition tt is Budget[tt] minus
	// the frozen Hamming there.  costAt[tt] = frozen[tt] + movInc[tt], where
	// movInc[tt] is the incumbent Hamming of the pool demands that can actually
	// change that transition.  halfMovable rounds 1 of a first-half run down to
	// half of the movable slice so one round cannot spend the whole budget.
	frozen    []int
	movInc    []int
	costAt    []int
	halfFirst bool
	// pinnedViol lists cells the caller called immovable but some candidate of
	// this pool can change (see BuildOptions.Pinned).
	pinnedViol []PinnedViolation
	// floors[ci] is the absolute load floor of tracked cell ci (0 = none) and
	// satFloor is the global first-bit floor; both come from the presolve.
	floors   []float64
	satFloor float64
}

// PinnedViolation is a pinned cell that a candidate of the pool moves, with the
// largest absolute change it would cause.
type PinnedViolation struct {
	Key   snap.Key
	Delta float64
}

// PinnedViolations returns the pinned cells that this pool can change, sorted
// by (slot, arc).  A non-empty result means the presolve proof for those cells
// is contradicted by the candidate pool; the caller should log it loudly.
func (pr *Problem) PinnedViolations() []PinnedViolation { return pr.pinnedViol }

// BuildOptions selects the Hamming-budget mode of a round.
type BuildOptions struct {
	// HalfFirst rounds the movable budget down to 50% (design doc §11
	// first_half: round 1 of a setB run spends half of the remaining movable
	// budget so early decisions keep headroom for later rounds).
	HalfFirst bool
	// Pinned lists the cells the presolve proved immovable (lb == ub).  A cell
	// no candidate touches is already outside `tracked`, so its saturation
	// enters constantFloor() and the first layer's z floor for free -- the pins
	// need no extra floor term.  This field only exists to *detect* a
	// contradiction: when a candidate would change a pinned cell the proof is
	// wrong, so the cell is reported through PinnedViolations instead of being
	// silently trusted (宁漏勿错).
	Pinned map[snap.Key]bool
	// Floors maps a cell to a load it can never drop below (presolve's sound
	// lower bounds, proven or not).  A tracked cell with a floor gets an extra
	// row, so the MIP's optimum can no longer be an unnaturally low value that
	// no routing can reach -- without it the first layer happily "improves" the
	// max by pushing a cell under its floor and the round is rejected for free.
	Floors map[snap.Key]float64
	// SatFloor is a lower bound on the saturation of the first bit, valid for
	// every routing (max over all cells of floor/load -- Bounds.SatFloor).  It
	// floors layer 0's z on top of constantFloor().
	SatFloor float64
}

// Build constructs the MIP problem from a pool with the full-budget mode.
func Build(p *Pool, sn *snap.Snap) (*Problem, error) {
	return buildMode(p, sn, BuildOptions{})
}

// BuildMode constructs the MIP problem from a pool under a Hamming budget mode
// (see BuildOptions).
func BuildMode(p *Pool, sn *snap.Snap, opts BuildOptions) (*Problem, error) {
	return buildMode(p, sn, opts)
}

// buildMode constructs the MIP problem from a pool under a budget mode.
func buildMode(p *Pool, sn *snap.Snap, opts BuildOptions) (*Problem, error) {
	m, T := p.m, p.T
	prob := &Problem{
		inst: p.inst, g: p.g, snap: sn, m: m, T: T,
		slots: p.Slots, pairs: p.Pairs, halfFirst: opts.HalfFirst,
		frozen: make([]int, T), movInc: make([]int, T), costAt: make([]int, T),
		satFloor: opts.SatFloor,
	}
	prob.pos = make(map[int]int, len(p.Slots))
	for i, s := range p.Slots {
		prob.pos[s] = i
	}

	// 1. Tracked cells: any (slot, arc) whose load differs between two
	//    candidates of the same pair, with the largest such difference kept for
	//    the pinned-cell check below (magnitude, not bitwise equality: two
	//    candidate loads are sums of the same volumes in a different order and
	//    may differ in the last ulp).
	diff := map[cell]float64{}
	cur := p.snap.Load() // arc-major [a*T + t]
	for _, pr := range p.Pairs {
		c0 := pr.Cand[0].Load
		for _, c := range pr.Cand[1:] {
			for si := range p.Slots {
				base := si * m
				for a := 0; a < m; a++ {
					d := c.Load[base+a] - c0[base+a]
					if d == 0 {
						continue
					}
					if d < 0 {
						d = -d
					}
					k := cell{p.Slots[si], a}
					if d > diff[k] {
						diff[k] = d
					}
				}
			}
		}
	}
	for c, dmax := range diff {
		if opts.Pinned[snap.Key{T: c.slot, A: c.a}] {
			// The presolve called this cell immovable.  A *real* move is a
			// contradiction (record it, and keep the cell tracked so the MIP is
			// never blinded by a suspect pin); float noise is not.
			load := cur[c.a*T+c.slot]
			if dmax > 1e-9*math.Max(1, math.Abs(load)) {
				prob.pinnedViol = append(prob.pinnedViol, PinnedViolation{
					Key: snap.Key{T: c.slot, A: c.a}, Delta: dmax,
				})
			} else {
				continue
			}
		}
		prob.tracked = append(prob.tracked, c)
	}
	sort.Slice(prob.pinnedViol, func(i, j int) bool {
		if prob.pinnedViol[i].Key.T != prob.pinnedViol[j].Key.T {
			return prob.pinnedViol[i].Key.T < prob.pinnedViol[j].Key.T
		}
		return prob.pinnedViol[i].Key.A < prob.pinnedViol[j].Key.A
	})
	sort.Slice(prob.tracked, func(i, j int) bool {
		if prob.tracked[i].slot != prob.tracked[j].slot {
			return prob.tracked[i].slot < prob.tracked[j].slot
		}
		return prob.tracked[i].a < prob.tracked[j].a
	})

	// 2. Delta vectors per pair per candidate and cell base loads.
	nCell := len(prob.tracked)
	for _, pr := range p.Pairs {
		c0 := pr.Cand[0].Load
		d := make([]float64, len(pr.Cand)*nCell)
		for k, c := range pr.Cand {
			for ci, cl := range prob.tracked {
				base := prob.pos[cl.slot] * m
				d[k*nCell+ci] = c.Load[base+cl.a] - c0[base+cl.a]
			}
		}
		prob.delta = append(prob.delta, d)
	}
	for _, cl := range prob.tracked {
		prob.caps = append(prob.caps, p.g.Cap[cl.a])
		prob.base = append(prob.base, cur[cl.a*T+cl.slot])
		prob.floors = append(prob.floors, opts.Floors[snap.Key{T: cl.slot, A: cl.a}])
	}

	// 3. Per-candidate Hamming deltas on every transition the move touches.
	for _, pr := range p.Pairs {
		costs := make([]map[int]int, len(pr.Cand))
		for k := range pr.Cand {
			costs[k] = map[int]int{}
		}
		for k, c := range pr.Cand {
			if k == 0 {
				continue // current routing: zero delta on every transition
			}
			for tt := 1; tt < T; tt++ {
				if d := prob.candidateTransDelta(pr.D, &c, tt); d != 0 {
					costs[k][tt] = d
				}
			}
		}
		prob.trans = append(prob.trans, costs)
	}

	// 4. Per-transition frozen / movable Hamming split (first-half budget mode).
	//    A pair demand is movable on tt when at least one of its alternatives
	//    changes the transition cost; its incumbent Hamming is movInc[tt].  The
	//    frozen Hamming frozen[tt] = total incumbent CostAt(tt) - movInc[tt]
	//    cannot be reallocated this round, so the movable slice is
	//    Budget[tt] - frozen[tt].
	for tt := 1; tt < T; tt++ {
		prob.costAt[tt] = sn.CostAt(tt)
		mi := 0
		for i, prr := range p.Pairs {
			movable := false
			for _, cm := range prob.trans[i] {
				if _, ok := cm[tt]; ok {
					movable = true
					break
				}
			}
			if movable {
				mi += snap.PathDist(prob.inst, prr.D,
					sn.GetWaypoints(prr.D, tt-1), sn.GetWaypoints(prr.D, tt))
			}
		}
		prob.movInc[tt] = mi
		prob.frozen[tt] = prob.costAt[tt] - mi
	}
	return prob, nil
}

// candidateTransDelta returns the change of the Hamming cost on transition tt
// (between slots tt-1 and tt) if this candidate is chosen over the current
// routing.  Slots the candidate does not move keep their current waypoints.
func (pr *Problem) candidateTransDelta(d int, c *Candidate, tt int) int {
	inst := pr.inst
	wpAt := func(s int) []int {
		if po, ok := pr.pos[s]; ok && c.Move[po] {
			return c.Wps
		}
		return pr.snap.GetWaypoints(d, s)
	}
	return snap.PathDist(inst, d, wpAt(tt-1), wpAt(tt)) -
		snap.PathDist(inst, d, pr.snap.GetWaypoints(d, tt-1), pr.snap.GetWaypoints(d, tt))
}

// Solve runs the lexicographic peeling MIP.  Layer 1 picks exactly one
// candidate per pair so that the maximum saturation over the tracked cells
// (one shared continuous z) is minimised.  Each later layer locks the cells
// that attained the previous optimum at their attained load (upper bound only,
// so they may still drop) and minimises a fresh z over the remaining cells,
// pushing down the second, third, ... entries of the vector in turn.
// Per-pair convexity and per-transition Hamming-budget rows are present in
// every layer.
//
// Result.Runtime sums the Gurobi Runtime of this call's layers; it is
// bookkeeping for the round loop and never feeds a solving decision.
func (pr *Problem) Solve(opts SolveOptions) (*Result, error) {
	if opts.MaxPeel <= 0 {
		opts.MaxPeel = 1
	}
	lockedLoad := map[cell]float64{}
	var best *Result
	totalRt := 0.0 // summed over this call's layers, see Result.Runtime
	for layer := 0; layer < opts.MaxPeel; layer++ {
		// Layer 1 models the true global first bit: z is floored by the max
		// saturation of every immutable (non-tracked) cell.  If that floor is
		// attained only by immutable cells (no tracked cell reaches z*), the
		// top tier is untouchable and later layers must be free to push the
		// tracked cells below it, so their z carries no floor.
		zlb := 0.0
		if layer == 0 {
			zlb = math.Max(pr.constantFloor(), pr.satFloor)
		}
		res, load, err := pr.solveLayer(lockedLoad, zlb, opts.TimeLimit)
		if err != nil {
			return nil, err
		}
		if res != nil {
			totalRt += res.Runtime
		}
		if res == nil || !res.HasValue || len(res.Choices) == 0 {
			if res != nil {
				res.Runtime = totalRt
			}
			return res, nil
		}
		best = res
		if layer == opts.MaxPeel-1 {
			break
		}
		// Peel: every still-active tracked cell whose saturation reaches the
		// current optimum z is pinned at its attained load and excluded from
		// the next layer's max.  A layer whose optimum is reached only by the
		// frozen background locks nothing and the peel simply moves on.
		for i, cl := range pr.tracked {
			if _, ok := lockedLoad[cl]; ok {
				continue
			}
			if load[i]/pr.caps[i] >= res.ObjVal-1e-7 {
				lockedLoad[cl] = load[i]
			}
		}
	}
	if best != nil {
		best.Runtime = totalRt
	}
	return best, nil
}

// solveLayer builds and solves one peel layer.  lockedLoad caps the load of
// tracked cells already assigned to earlier layers (absolute upper-bound rows);
// zlb is this layer's z lower bound (constantFloor for layer 1, 0 afterwards).
// timeLimit bounds the Gurobi solve (0 = solve to completion; a hit surfaces as
// StatusInterrupted and the incumbent, if any, is used).
// It returns the result and the load attained on each tracked cell.
func (pr *Problem) solveLayer(lockedLoad map[cell]float64, zlb float64, timeLimit time.Duration) (*Result, []float64, error) {
	inst := pr.inst

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
	if timeLimit > 0 {
		// TimeLimit is honoured by Gurobi even while a long LP relaxation is
		// running, which GRBterminate (checked only at node boundaries) is not.
		if err := env.SetParam("TimeLimit", fmt.Sprintf("%g", timeLimit.Seconds())); err != nil {
			return nil, nil, err
		}
	}
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
	obj[zIdx] = 1
	if _, err := model.AddVars(obj, lb, ub, vt); err != nil {
		return nil, nil, err
	}

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
			return
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

	// Cell rows.  Current total load on tracked cell ci is base[ci]; each
	// chosen candidate changes it by delta, so the new load is
	//   base + sum(delta * x).
	// Unlocked: base + sum(delta*x) <= cap*z  ->  sum(delta*x) - cap*z <= -base.
	// Locked:   base + sum(delta*x) <= lock    ->  sum(delta*x) <= lock - base.
	nCell := len(pr.tracked)
	for ci, cl := range pr.tracked {
		var cols []int
		var vals []float64
		for i, prr := range pr.pairs {
			d := pr.delta[i]
			for k := range prr.Cand {
				dk := d[k*nCell+ci]
				if dk != 0 {
					cols = append(cols, base[i]+k)
					vals = append(vals, dk)
				}
			}
		}
		// Absolute floor of the cell (presolve): no routing goes below it, so
		// demanding it of the MIP is sound and keeps the reported optimum
		// reachable.  Added for locked cells too: the extra row only prunes.
		if fl := pr.floors[ci]; fl > 0 {
			lo := make([]int, len(cols))
			copy(lo, cols)
			loV := make([]float64, len(vals))
			copy(loV, vals)
			addRow(lo, loV, '>', fl-pr.base[ci])
		}
		if lock, ok := lockedLoad[cl]; ok {
			addRow(cols, vals, '<', lock-pr.base[ci])
		} else {
			cols = append(cols, zIdx)
			vals = append(vals, -pr.caps[ci])
			addRow(cols, vals, '<', -pr.base[ci])
		}
	}

	// Hamming-budget rows for every transition any candidate affects.
	for tt := 1; tt < pr.T; tt++ {
		budget := -1
		if tt < len(inst.Scenario.Budget) {
			budget = inst.Scenario.Budget[tt]
		}
		if budget < 0 {
			continue
		}
		var cols []int
		var vals []float64
		for i := range pr.pairs {
			costs := pr.trans[i]
			for k, cm := range costs {
				dv, ok := cm[tt]
				if !ok || dv == 0 {
					continue
				}
				cols = append(cols, base[i]+k)
				vals = append(vals, float64(dv))
			}
		}
		if len(cols) == 0 {
			continue
		}
		rhs := float64(budget - pr.snap.CostAt(tt))
		if pr.halfFirst {
			// first_half (design doc §11): round 1 may spend only half of the
			// movable slice Budget - frozen (the frozen demands' Hamming is
			// already committed and subtracted); clamp the delta row at 0 so an
			// incumbent movable cost above the half-slice keeps the model
			// feasible (no increase) instead of infeasible.
			rhs = math.Floor(0.5*float64(budget-pr.frozen[tt])) - float64(pr.movInc[tt])
			if rhs < 0 {
				rhs = 0
			}
		}
		addRow(cols, vals, '<', rhs)
	}

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
	if err = model.Optimize(); err != nil {
		return nil, nil, err
	}
	status, err := model.IntAttr("Status")
	if err != nil {
		return nil, nil, err
	}
	res := &Result{Status: status}
	// Gurobi's own Runtime attribute: how long the optimizer ran, excluding the
	// model build above.  Best-effort -- a wrapper that cannot read it leaves the
	// field at zero rather than failing the solve.
	if rt, err := model.DblAttr("Runtime"); err == nil {
		res.Runtime = rt
	}
	if status != gurobi.StatusOptimal && status != gurobi.StatusTimeLimit &&
		status != gurobi.StatusSuboptimal && status != gurobi.StatusInterrupted {
		return res, nil, nil
	}
	res.HasValue = true
	if status != gurobi.StatusOptimal {
		// Interrupted before the first incumbent leaves no X vector to read.
		n, err := model.IntAttr("SolCount")
		if err != nil {
			return nil, nil, err
		}
		if n <= 0 {
			return res, nil, nil
		}
	}
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
		ch := Choice{D: prr.D}
		for si := range pr.slots {
			if prr.Cand[best].Move[si] {
				ch.Apply = append(ch.Apply, SlotWps{Slot: pr.slots[si], Wps: prr.Cand[best].Wps})
			}
		}
		res.Choices = append(res.Choices, ch)
	}
	load := make([]float64, nCell)
	for ci := range pr.tracked {
		l := pr.base[ci]
		for i := range pr.pairs {
			d := pr.delta[i]
			for k := range pr.pairs[i].Cand {
				l += d[k*nCell+ci] * xv[base[i]+k]
			}
		}
		load[ci] = l
	}
	return res, load, nil
}

// layerModel is a built but unsolved peel layer, plus the index bookkeeping
// needed to read an answer back out of it.  It owns the Gurobi environment and
// model: call Free when done.
type layerModel struct {
	env   *gurobi.Env
	model *gurobi.Model
	nX    int
	zIdx  int
	base  []int
	// cellRow[ci] is the constraint row carrying tracked cell ci's load
	// equation, or -1 when the cell contributes no nonzeros and so gets no row
	// at all.  The dual probe reads Pi off exactly these rows.
	cellRow []int
	// pairRow lists the per-demand convexity rows.  The probe reads Pi off them
	// as a smoke test: if these price at zero too, a zero cell price says
	// nothing about the cells.
	pairRow []int
}

// Free releases the model and then the environment.
func (lm *layerModel) Free() {
	if lm.model != nil {
		lm.model.Free()
		lm.model = nil
	}
	if lm.env != nil {
		lm.env.Free()
		lm.env = nil
	}
}

// newLayer builds one peel layer.  lockedLoad caps the load of tracked cells
// already assigned to earlier layers (absolute upper-bound rows); zlb is this
// layer's z lower bound (constantFloor for layer 1, 0 afterwards).  timeLimit
// bounds the Gurobi solve (0 = solve to completion; a hit surfaces as
// StatusInterrupted and the incumbent, if any, is used).
//
// relax replaces every variable's type with 'C', bounds unchanged: that is the
// LP relaxation the dual probe takes prices from.  The row layout is identical
// in both modes, so cellRow addresses the same row either way.
func (pr *Problem) newLayer(lockedLoad map[cell]float64, zlb float64, timeLimit time.Duration, relax bool) (*layerModel, error) {
	inst := pr.inst

	nX := 0
	for _, prr := range pr.pairs {
		nX += len(prr.Cand)
	}
	zIdx := nX
	if nX == 0 {
		return nil, fmt.Errorf("mip: no candidate variables")
	}

	env, err := gurobi.EnvNew()
	if err != nil {
		return nil, err
	}
	if timeLimit > 0 {
		// TimeLimit is honoured by Gurobi even while a long LP relaxation is
		// running, which GRBterminate (checked only at node boundaries) is not.
		if err := env.SetParam("TimeLimit", fmt.Sprintf("%g", timeLimit.Seconds())); err != nil {
			env.Free()
			return nil, err
		}
	}
	model, err := env.NewModel("mip")
	if err != nil {
		env.Free()
		return nil, err
	}
	lm := &layerModel{env: env, model: model, nX: nX, zIdx: zIdx}
	built := false
	defer func() {
		if !built {
			lm.Free()
		}
	}()

	if err := model.SetIntAttr("ModelSense", gurobi.ModelSenseMinimize); err != nil {
		return nil, err
	}

	obj := make([]float64, nX+1)
	lb := make([]float64, nX+1)
	ub := make([]float64, nX+1)
	vt := make([]byte, nX+1)
	for i := 0; i < nX; i++ {
		if relax {
			vt[i] = 'C'
		} else {
			vt[i] = 'B'
		}
		ub[i] = 1
	}
	vt[zIdx] = 'C'
	ub[zIdx] = 1e30
	lb[zIdx] = zlb
	obj[zIdx] = 1
	if _, err := model.AddVars(obj, lb, ub, vt); err != nil {
		return nil, err
	}

	base := make([]int, len(pr.pairs))
	acc := 0
	for i, prr := range pr.pairs {
		base[i] = acc
		acc += len(prr.Cand)
	}
	lm.base = base

	var cbeg []int32
	var cind []int32
	var cval []float64
	var sense []byte
	var rhs []float64
	// addRow returns the new row's index, or -1 when the row was empty and so
	// not added at all (Gurobi rejects an all-zero row).
	addRow := func(cols []int, vals []float64, s byte, r float64) int {
		if len(cols) == 0 {
			return -1
		}
		cbeg = append(cbeg, int32(len(cind)))
		for j, col := range cols {
			cind = append(cind, int32(col))
			cval = append(cval, vals[j])
		}
		sense = append(sense, s)
		rhs = append(rhs, r)
		return len(sense) - 1
	}

	// Per-demand convexity: exactly one candidate column per pair, with column 0
	// -- the "current routing" reference whose Move mask is empty -- always in
	// the row.  So a demand either keeps its routing or takes exactly one of the
	// pool's moves; it cannot take two at once, which is what keeps the
	// precomputed per-move saturation deltas exact and additive per slot.
	//
	// A set-packing variant (one row per (pair, transition), no column forcing a
	// choice) was tried on 2026-09-10 and reverted: on setA-01 it cost 1.98%
	// relative on the layer-2 component versus this row, because dropping the
	// forced move removes the pressure that flattens the tail.  Its only win was
	// 4.5e-5 relative on setB-01's max.  See experiments/2026-09-11_01_*.
	for i, prr := range pr.pairs {
		cols := make([]int, len(prr.Cand))
		vals := make([]float64, len(prr.Cand))
		for k := range prr.Cand {
			cols[k] = base[i] + k
			vals[k] = 1
		}
		lm.pairRow = append(lm.pairRow, addRow(cols, vals, '=', 1))
	}

	// Cell rows.  Current total load on tracked cell ci is base[ci]; each
	// chosen candidate changes it by delta, so the new load is
	//   base + sum(delta * x).
	// Unlocked: base + sum(delta*x) <= cap*z  ->  sum(delta*x) - cap*z <= -base.
	// Locked:   base + sum(delta*x) <= lock    ->  sum(delta*x) <= lock - base.
	nCell := len(pr.tracked)
	lm.cellRow = make([]int, nCell)
	for ci, cl := range pr.tracked {
		var cols []int
		var vals []float64
		for i, prr := range pr.pairs {
			d := pr.delta[i]
			for k := range prr.Cand {
				dk := d[k*nCell+ci]
				if dk != 0 {
					cols = append(cols, base[i]+k)
					vals = append(vals, dk)
				}
			}
		}
		// Absolute floor of the cell (presolve): no routing goes below it, so
		// demanding it of the MIP is sound and keeps the reported optimum
		// reachable.  Added for locked cells too: the extra row only prunes.
		if fl := pr.floors[ci]; fl > 0 {
			lo := make([]int, len(cols))
			copy(lo, cols)
			loV := make([]float64, len(vals))
			copy(loV, vals)
			addRow(lo, loV, '>', fl-pr.base[ci])
		}
		if lock, ok := lockedLoad[cl]; ok {
			lm.cellRow[ci] = addRow(cols, vals, '<', lock-pr.base[ci])
		} else {
			cols = append(cols, zIdx)
			vals = append(vals, -pr.caps[ci])
			lm.cellRow[ci] = addRow(cols, vals, '<', -pr.base[ci])
		}
	}

	// Hamming-budget rows for every transition any candidate affects.
	for tt := 1; tt < pr.T; tt++ {
		budget := -1
		if tt < len(inst.Scenario.Budget) {
			budget = inst.Scenario.Budget[tt]
		}
		if budget < 0 {
			continue
		}
		var cols []int
		var vals []float64
		for i := range pr.pairs {
			costs := pr.trans[i]
			for k, cm := range costs {
				dv, ok := cm[tt]
				if !ok || dv == 0 {
					continue
				}
				cols = append(cols, base[i]+k)
				vals = append(vals, float64(dv))
			}
		}
		if len(cols) == 0 {
			continue
		}
		rhs := float64(budget - pr.snap.CostAt(tt))
		if pr.halfFirst {
			// first_half (design doc §11): round 1 may spend only half of the
			// movable slice Budget - frozen (the frozen demands' Hamming is
			// already committed and subtracted); clamp the delta row at 0 so an
			// incumbent movable cost above the half-slice keeps the model
			// feasible (no increase) instead of infeasible.
			rhs = math.Floor(0.5*float64(budget-pr.frozen[tt])) - float64(pr.movInc[tt])
			if rhs < 0 {
				rhs = 0
			}
		}
		addRow(cols, vals, '<', rhs)
	}

	cbeg = append(cbeg, int32(len(cind)))
	if err := model.AddConstrs(cbeg, cind, cval, sense, rhs); err != nil {
		return nil, err
	}
	if err := model.Update(); err != nil {
		return nil, err
	}
	if lp := os.Getenv("TASR_GRB_LP"); lp != "" {
		if err := model.Write(lp); err != nil {
			return nil, err
		}
	}
	built = true
	return lm, nil
}

// optimize solves the current state of an already-built layer model and reads
// the chosen moves back out of it.  It returns the result and the load attained
// on each tracked cell.
func (pr *Problem) optimize(lm *layerModel) (*Result, []float64, error) {
	model, nX, base := lm.model, lm.nX, lm.base

	if err := model.Optimize(); err != nil {
		return nil, nil, err
	}
	status, err := model.IntAttr("Status")
	if err != nil {
		return nil, nil, err
	}
	res := &Result{Status: status}
	// Gurobi's own Runtime attribute: how long the optimizer ran, excluding the
	// model build in newLayer.  Best-effort -- a wrapper that cannot read it
	// leaves the field at zero rather than failing the solve.
	if rt, err := model.DblAttr("Runtime"); err == nil {
		res.Runtime = rt
	}
	if status != gurobi.StatusOptimal && status != gurobi.StatusTimeLimit &&
		status != gurobi.StatusSuboptimal && status != gurobi.StatusInterrupted {
		return res, nil, nil
	}
	res.HasValue = true
	if status != gurobi.StatusOptimal {
		// Interrupted before the first incumbent leaves no X vector to read.
		n, err := model.IntAttr("SolCount")
		if err != nil {
			return nil, nil, err
		}
		if n <= 0 {
			return res, nil, nil
		}
	}
	objVal, err := model.DblAttr("ObjVal")
	if err != nil {
		return nil, nil, err
	}
	res.ObjVal = objVal
	xv := make([]float64, nX)
	if err := model.X(xv); err != nil {
		return nil, nil, err
	}
	// Every selected column is read back rather than taking the argmax.  The
	// convexity row leaves at most one set per pair, so the two agree today --
	// but a readback that cannot silently drop a selection is the right thing to
	// have if the pair row is ever loosened again, and a MIP solution at the
	// tolerance boundary is exactly where an argmax would drop one.  Column 0 is
	// the "current routing" reference: it carries no move and is never selected.
	for i, prr := range pr.pairs {
		ch := Choice{D: prr.D}
		for k := 1; k < len(prr.Cand); k++ {
			if xv[base[i]+k] < 0.5 {
				continue
			}
			for si, moved := range prr.Cand[k].Move {
				if moved {
					ch.Apply = append(ch.Apply, SlotWps{Slot: pr.slots[si], Wps: prr.Cand[k].Wps})
				}
			}
		}
		if len(ch.Apply) > 0 {
			res.Choices = append(res.Choices, ch)
		}
	}
	load := pr.loadsFromX(lm, xv)
	return res, load, nil
}

// loadsFromX turns a variable vector into each tracked cell's load.  It is
// shared by the integer peel and the LP probe so both read a solution the same
// way -- the probe's locking decisions are only meaningful against the solver's
// own arithmetic.
func (pr *Problem) loadsFromX(lm *layerModel, xv []float64) []float64 {
	nCell := len(pr.tracked)
	load := make([]float64, nCell)
	for ci := range pr.tracked {
		l := pr.base[ci]
		for i := range pr.pairs {
			d := pr.delta[i]
			for k := range pr.pairs[i].Cand {
				l += d[k*nCell+ci] * xv[lm.base[i]+k]
			}
		}
		load[ci] = l
	}
	return load
}

// constantFloorCell returns the cell whose saturation sets the constant floor,
// alongside the value.  The pair matters more than the number: the floor is by
// construction a cell that no candidate column of this pool can move, so when it
// sits above the presolve's sound floor (Bounds.SatFloor) it names the exact cell
// the candidate layer is failing to cover -- the difference between "the search
// cannot find the routing" and "the pool contains no such routing".
func (pr *Problem) constantFloorCell() (snap.Key, float64) {
	sat := pr.snap.Saturations()
	m, T := pr.m, pr.T
	tracked := map[cell]bool{}
	for _, cl := range pr.tracked {
		tracked[cl] = true
	}
	var best snap.Key
	floor := 0.0
	for a := 0; a < m; a++ {
		for tt := 0; tt < T; tt++ {
			if tracked[cell{tt, a}] {
				continue
			}
			if sat[a*T+tt] > floor {
				floor = sat[a*T+tt]
				best = snap.Key{T: tt, A: a}
			}
		}
	}
	return best, floor
}

// constantFloor returns the maximum saturation over all cells that cannot
// change (every (slot, arc) not among the tracked cells of this pool).
func (pr *Problem) constantFloor() float64 {
	_, floor := pr.constantFloorCell()
	return floor
}
