package mip

import (
	"math"
	"sort"
	"time"

	"tasr/internal/gurobi"
	"tasr/internal/snap"
)

// The dual probe: read the LP prices of the tracked cells instead of solving
// the integer model.  It is the cheap first step of the dual-candidate plan and
// is deliberately observation-only -- nothing here feeds a decision, so it can
// be run against any round without changing what the solver does.
//
// Why prices are a different signal from what the outer loop already has: the
// outer loop ranks seeds by saturation (load/capacity), which is layer-1
// information -- the max load.  Measured on setA, layer 1 ties the reference on
// 13/20 instances while the full vector ties 0/20, i.e. the remaining gap sits
// almost entirely *after* the first layer.  Once the first bit is pinned on an
// arc a1, no amount of further work on a1 moves layer 2; layer 2's bottleneck is
// a different arc a2.  The price Pi of a cell's load row is precisely "what is
// one unit of load on this cell worth at this layer", and it is defined for
// every tracked cell at once -- including the ones saturation ranks low.
//
// Pi is only defined for a pure LP with an optimal basis, so the probe builds a
// continuous copy of the pool (every variable 'C', bounds unchanged) and reports
// prices only when Gurobi returns Optimal.  On any other status the report comes
// back with Proven == false and no prices, which is the same proven/not-proven
// split the confidence memory already uses (Result.Proven).

// CellPrice is one tracked cell's LP price.
type CellPrice struct {
	Key  snap.Key
	Cap  float64
	Base float64
	// Sat is base/cap: the saturation the outer loop ranks seeds by, kept here
	// so the price can be compared against the signal it would replace.
	Sat float64
	// Pi is the dual of this cell's load row at the probed layer.
	Pi float64
	// Load and LoadSat are the cell's load and saturation at the probed layer's
	// LP optimum.  Sat above is the *current snapshot's* saturation -- the
	// signal the seed ranking already uses.  Comparing the two separates the two
	// roles a cell can play at a layer: LoadSat at the layer's Zlp means the
	// layer's max is decided on this cell (it is about to be locked, and it is
	// the one whose price is non-zero), while LoadSat below Zlp means the layer
	// left it slack and complementary slackness forces its price to zero.
	Load    float64
	LoadSat float64
}

// DualReport is one probe run's output.
type DualReport struct {
	// Status is the Gurobi status of the LP solve.
	Status int
	// Proven is true when the LP was solved to optimality, i.e. when Pi is
	// defined.  False means the prices below are absent, not zero.
	Proven bool
	// Zlp is the LP relaxation's objective (the minimum max-saturation the
	// fractional pool can reach); compare against the integer optimum to see how
	// much of the round's decision the relaxation is making up.
	Zlp float64
	// Cells carries one entry per tracked cell that got a load row.
	Cells []CellPrice
	// PairPi is the price of each pair's convexity row and PairNonZero counts
	// the non-trivial ones.  These rows are equalities, so they are active no
	// matter what the routing does; they are the probe's smoke test.  All-zero
	// cell prices mean something completely different depending on this field:
	// with live pair prices the zeroes are a real degeneracy (every cell row is
	// slack, i.e. no tracked cell is tight at this layer), while with dead pair
	// prices the probe is reading the wrong attribute and nothing it prints can
	// be trusted.
	PairPi      []float64
	PairNonZero int
	// PairAbsMax is the largest |Pi| over the convexity rows.  It is what makes
	// PairNonZero readable: an active equality's dual is *allowed* to be zero, so
	// a run of exact zeros is a legitimate degenerate basis rather than proof of
	// a misread.  What separates the two cases is the magnitude -- a real read of
	// an all-zero dual returns exact 0.0, whereas reading the wrong attribute or
	// the wrong index space leaves solver-scaled noise (1e-13, 1e-11) behind.
	PairAbsMax float64
	// CellAbsMax is the largest |Pi| over the priced cells, and CellLive /
	// CellNaN count the cells whose price is non-trivial and non-finite.  They
	// exist because a dual vector from a degenerate LP is not automatically a
	// well-behaved one: Gurobi is free to hand back NaN for a price it cannot
	// pin down, and a NaN silently defeats every comparison written against it
	// (NaN > x is false, so it counts as zero in a "non-zero" tally and never
	// wins a max).  Measuring them first is the difference between "the prices
	// are zero" and "the prices are not numbers".
	CellAbsMax float64
	CellLive   int
	// SumAbsPiCap is sum(|Pi| * Cap) over the rows that still carry z, and it is
	// the probe's real correctness check.  Each unlocked cell's row is
	// sum(delta*x) - cap*z <= -base, so stationarity in z forces
	// sum(lambda_i * cap_i) = -1 over those rows, i.e. SumAbsPiCap == 1.  It is
	// also what explains prices that look wrong: a cell whose |Pi| != 1/Cap is
	// not a misread but a *shared* price, because when several cells bind at the
	// layer's optimum the LP may split the total arbitrarily among them and only
	// the cap-weighted sum is invariant.  Locked rows drop z and are excluded --
	// their prices are a different quantity and would break the sum.
	//
	// Expect 1 only when z is interior: at layer 0 z sits on its floor, z is
	// non-basic, and the identity does not apply.
	SumAbsPiCap float64
	CellNaN     int
	PairNaN     int
	// FloorKey / FloorSat name the cell that sets this pool's constant floor:
	// the highest-saturation cell no candidate column can move.  When FloorSat
	// is at or above Zlp the LP is not choosing anything -- z is pinned by a cell
	// outside the pool's reach, and every cell price is then zero by
	// construction.  Comparing FloorSat against the presolve's sound floor turns
	// that from a dead end into a lead: the difference is exactly the candidate
	// layer's blind spot, on a named arc.
	FloorKey snap.Key
	FloorSat float64
}

// piLive is the threshold below which a price is treated as exactly zero.
// Degenerate LPs return long runs of exact 0.0, so this only has to be loose
// enough not to let solver noise in; CellLive counts with it and Disagreement
// caps k by it.
const piLive = 1e-9

// DualProbe solves the pool as a pure LP relaxation with z floored at zlb and
// reads every tracked cell's load-row price.  A zero TimeLimit solves to
// completion.
func (pr *Problem) DualProbe(zlb float64, timeLimit time.Duration) (*DualReport, error) {
	lm, err := pr.newLayer(nil, zlb, timeLimit, true)
	if err != nil {
		return nil, err
	}
	defer lm.Free()
	rep, _, err := pr.solveProbeLayer(lm, nil)
	return rep, err
}

// DualProbeFirstLayer probes with the same z floor the first peel layer uses, so
// the prices describe the LP the solver's own layer 1 sees rather than a
// relaxed variant of it.
func (pr *Problem) DualProbeFirstLayer(timeLimit time.Duration) (*DualReport, error) {
	return pr.DualProbe(math.Max(pr.constantFloor(), pr.satFloor), timeLimit)
}

// solveProbeLayer optimizes an already-built layer model, reads the prices, and
// returns each tracked cell's attained load alongside the report.  The load
// vector is what the peel needs to decide which cells to lock; both the plain
// probe and the peel probe go through here so they cannot drift apart.
func (pr *Problem) solveProbeLayer(lm *layerModel, lockedBefore map[cell]float64) (*DualReport, []float64, error) {
	rep := &DualReport{Status: -1}
	rep.FloorKey, rep.FloorSat = pr.constantFloorCell()

	if err := lm.model.Optimize(); err != nil {
		return nil, nil, err
	}
	st, err := lm.model.IntAttr("Status")
	if err != nil {
		return nil, nil, err
	}
	rep.Status = st
	if st != gurobi.StatusOptimal {
		return rep, nil, nil
	}
	rep.Proven = true
	if rep.Zlp, err = lm.model.DblAttr("ObjVal"); err != nil {
		return nil, nil, err
	}
	xv := make([]float64, lm.nX)
	if err := lm.model.X(xv); err != nil {
		return nil, nil, err
	}
	load := pr.loadsFromX(lm, xv)

	// Only cells that contribute nonzeros have a row; a cell whose row was
	// empty has no price to read.  cellRow is built in tracked order, so the
	// surviving indices are already ascending, which GRBgetdblattrlist requires.
	idx := make([]int32, 0, len(lm.cellRow))
	cellOf := make([]int, 0, len(lm.cellRow))
	for ci, row := range lm.cellRow {
		if row < 0 {
			continue
		}
		idx = append(idx, int32(row))
		cellOf = append(cellOf, ci)
	}
	pi := make([]float64, len(idx))
	if len(idx) > 0 {
		if err := lm.model.GetDblAttrList("Pi", idx, pi); err != nil {
			return nil, nil, err
		}
	}

	// The convexity rows are equalities, so they are active by construction, but
	// complementary slackness only forces a *slack* row to price zero -- an active
	// equality is still free to price zero, and it usually is: most setA layers
	// report 0/N here.  But not always (setA-04 prices 2/33 with |Pi| up to 0.12,
	// setA-10/13/16/19 price one or two), so these are not the pass/fail test the
	// "active therefore non-zero" reading would suggest.  They are read for their
	// magnitude, which is what separates a genuine all-zero dual from a wrong
	// attribute or index space.  The layer's real correctness check is
	// SumAbsPiCap.
	if n := len(lm.pairRow); n > 0 {
		pidx := make([]int32, 0, n)
		for _, row := range lm.pairRow {
			if row >= 0 {
				pidx = append(pidx, int32(row))
			}
		}
		if len(pidx) > 0 {
			rep.PairPi = make([]float64, len(pidx))
			if err := lm.model.GetDblAttrList("Pi", pidx, rep.PairPi); err != nil {
				return nil, nil, err
			}
			for _, v := range rep.PairPi {
				switch {
				case math.IsNaN(v) || math.IsInf(v, 0):
					rep.PairNaN++
				default:
					if a := math.Abs(v); a > rep.PairAbsMax {
						rep.PairAbsMax = a
					}
					if math.Abs(v) > piLive {
						rep.PairNonZero++
					}
				}
			}
		}
	}

	rep.Cells = make([]CellPrice, 0, len(cellOf))
	for j, ci := range cellOf {
		cl := pr.tracked[ci]
		sat := 0.0
		if c := pr.caps[ci]; c > 0 {
			sat = pr.base[ci] / c
		}
		lsat := 0.0
		if c := pr.caps[ci]; c > 0 {
			lsat = load[ci] / c
		}
		rep.Cells = append(rep.Cells, CellPrice{
			Key:     snap.Key{T: cl.slot, A: cl.a},
			Cap:     pr.caps[ci],
			Base:    pr.base[ci],
			Sat:     sat,
			Pi:      pi[j],
			Load:    load[ci],
			LoadSat: lsat,
		})
	}
	for j, c := range rep.Cells {
		switch {
		case math.IsNaN(c.Pi) || math.IsInf(c.Pi, 0):
			rep.CellNaN++
		default:
			if _, wasLocked := lockedBefore[pr.tracked[cellOf[j]]]; !wasLocked {
				rep.SumAbsPiCap += math.Abs(c.Pi) * c.Cap
			}
			if a := math.Abs(c.Pi); a > rep.CellAbsMax {
				rep.CellAbsMax = a
			}
			if math.Abs(c.Pi) > piLive {
				rep.CellLive++
			}
		}
	}
	return rep, load, nil
}

// LockedCell is one cell a peel layer pinned: its attained load at that layer's
// optimum, which every later layer must respect as an upper bound.
type LockedCell struct {
	Key  snap.Key
	Load float64
	Sat  float64
}

// LayerProbe is one layer of an LP peel.
type LayerProbe struct {
	// Layer is the 0-based peel depth.
	Layer int
	// Zlb is the floor this layer's z was given (non-zero only for layer 0,
	// mirroring Solve: later layers must be free to push the already-locked
	// cells below the floor, so their z carries none).
	Zlb float64
	// Report is the layer's LP optimum and prices.  A layer with Report.Proven
	// false produced no prices and ends the peel.
	Report *DualReport
	// Locked lists the cells this layer pinned on the way to the next one.
	Locked []LockedCell
	// Repeated is true when this layer locked nothing, so the next layer would
	// re-solve the identical LP.  The peel stops there: a repeated layer is not
	// a new source of information, it is the same one printed twice.
	Repeated bool
}

// DualProbePeel runs the LP relaxation of the peel itself and reports the prices
// at each layer, cheapest-first: layer 0 is the model the solver's own first
// layer sees and each layer after it pins the cells that reached that layer's
// max and re-asks the question with them held fixed.
//
// This is the layer where the price has something to say.  At layer 0 z sits on
// the presolve floor on every instance measured, no cell row has to bind, and
// the dual is identically zero by construction -- the prices restate the seed
// ranking and nothing more.  A later layer has its z freed (lb 0) and its
// predecessors pinned as hard rows, so its optimum is decided by whichever cell
// is now the bottleneck, and that cell's price is the marginal value of load on
// it.  That is layer-2 information, which the outer loop's saturation ranking
// cannot see.
func (pr *Problem) DualProbePeel(layers int, timeLimit time.Duration) ([]LayerProbe, error) {
	if layers <= 0 {
		layers = 1
	}
	locked := map[cell]float64{}
	out := make([]LayerProbe, 0, layers)
	for layer := 0; layer < layers; layer++ {
		zlb := 0.0
		if layer == 0 {
			zlb = math.Max(pr.constantFloor(), pr.satFloor)
		}
		lm, err := pr.newLayer(locked, zlb, timeLimit, true)
		if err != nil {
			return nil, err
		}
		lrep := LayerProbe{Layer: layer, Zlb: zlb}
		rep, load, err := pr.solveProbeLayer(lm, locked)
		lm.Free()
		if err != nil {
			return nil, err
		}
		lrep.Report = rep
		out = append(out, lrep)
		if !rep.Proven {
			break
		}
		// Peel exactly as Solve does, on the LP's attained loads instead of the
		// MIP's.  A layer whose optimum is reached by no tracked cell (it is
		// decided outside the pool) locks nothing, and re-solving would print
		// the same prices again.
		for i, cl := range pr.tracked {
			if _, ok := locked[cl]; ok {
				continue
			}
			if pr.caps[i] <= 0 {
				continue
			}
			if load[i]/pr.caps[i] >= rep.Zlp-1e-7 {
				locked[cl] = load[i]
				out[len(out)-1].Locked = append(out[len(out)-1].Locked, LockedCell{
					Key: snap.Key{T: cl.slot, A: cl.a}, Load: load[i], Sat: load[i] / pr.caps[i],
				})
			}
		}
		if len(out[len(out)-1].Locked) == 0 {
			out[len(out)-1].Repeated = true
			break
		}
	}
	return out, nil
}

// OrderByPi returns the priced cells sorted by decreasing price.  Ties break on
// the cell key so the order is deterministic run to run: degenerate LPs are the
// norm here, and an unstable order would read as signal.
func (r *DualReport) OrderByPi() []CellPrice {
	out := append([]CellPrice(nil), r.Cells...)
	sort.SliceStable(out, func(i, j int) bool {
		// NaNs sort last and never compare as greater, so a non-finite price
		// cannot masquerade as the hottest cell while still being visible.
		ni := math.IsNaN(out[i].Pi)
		nj := math.IsNaN(out[j].Pi)
		if ni != nj {
			return nj
		}
		if out[i].Pi != out[j].Pi {
			return out[i].Pi > out[j].Pi
		}
		if out[i].Key.T != out[j].Key.T {
			return out[i].Key.T < out[j].Key.T
		}
		return out[i].Key.A < out[j].Key.A
	})
	return out
}

// OrderBySat returns the priced cells sorted by decreasing saturation, i.e. the
// order the outer loop's seed ranking is built on.
func (r *DualReport) OrderBySat() []CellPrice {
	out := append([]CellPrice(nil), r.Cells...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Sat != out[j].Sat {
			return out[i].Sat > out[j].Sat
		}
		if out[i].Key.T != out[j].Key.T {
			return out[i].Key.T < out[j].Key.T
		}
		return out[i].Key.A < out[j].Key.A
	})
	return out
}

// Disagreement counts how many of the cells the price would nominate are not in
// saturation's top-k, and reports how many nominations there were to compare.
// This is the probe's headline number: saturation ranks by layer-1 information,
// so a large disagreement is direct evidence that the price carries something
// the seed ranking does not already have.  A disagreement near zero would mean
// the dual is just re-deriving the ordering we already use.
//
// The two sides are deliberately asymmetric.  The price nominates at most
// min(k, live) cells, where live counts |Pi| > piLive, because a degenerate LP
// hands back long runs of exact zeros and the order within one of those runs is
// the tie-break, not a price -- counting a tie as a nomination would manufacture
// exactly the signal this is supposed to measure (the first probe run reported
// 8/10 that way with one real price).  Saturation still gets its full top-k, so
// the question asked is the meaningful one: does the price put anything in the
// top k that saturation left out?  Measuring "how many of the top-k by price are
// outside the top-k by saturation" against a shrunken k would instead ask
// whether the price's first pick equals saturation's first pick.
func (r *DualReport) Disagreement(k int) (off, considered int) {
	if k <= 0 {
		return 0, 0
	}
	topPi := r.OrderByPi()
	topSat := r.OrderBySat()
	if k > len(topSat) {
		k = len(topSat)
	}
	live := 0
	for _, c := range topPi {
		if math.Abs(c.Pi) > piLive {
			live++
		}
	}
	considered = live
	if considered > k {
		considered = k
	}
	if considered == 0 {
		return 0, 0
	}
	inSat := make(map[snap.Key]bool, k)
	for i := 0; i < k; i++ {
		inSat[topSat[i].Key] = true
	}
	// Walk the price ordering and take the first `considered` *live* entries --
	// not the first `considered` entries, which would be whatever zero-priced
	// cells sit at the top of the order.  A price can be live and negative (a
	// cell that must not gain load prices below zero), in which case it ranks
	// last and a plain prefix of the ordering would miss it entirely.
	taken := 0
	for _, c := range topPi {
		if math.Abs(c.Pi) <= piLive {
			continue
		}
		if !inSat[c.Key] {
			off++
		}
		taken++
		if taken == considered {
			break
		}
	}
	return off, considered
}
