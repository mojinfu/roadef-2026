package mip

import (
	"fmt"
	"sort"

	"tasr/internal/cand"
)

// Sticky-span semantics (design doc §12, user's V1.0): a round only makes
// decisions at a seed slot t.  The chosen waypoint list is then applied
// verbatim to the following consecutive slots that carry no independent
// decision this round (usually t+1).  All run slots share the same candidate
// column x, so the inter-slot Hamming of a run stays 0 and twins are the
// emergent special case (span reaches exactly the next slot), not a
// deliberately enumerated shape.
//
// The copy is an *override*: because rounds apply in schedule order, the seed
// decision of the current round is the newest action and supersedes whatever
// routing a previous round left on the slots it reaches (that routing was an
// older decision).  The run therefore does not stop at slots whose incumbent
// waypoint list differs — rewriting those is exactly how a zero-budget merge of
// two regimes is expressed.  It stops only at the span cap, the time horizon,
// and the demand's first zero-volume slot (a waypoint copied onto a slot that
// carries no traffic can only burn budget on the boundary transitions).
//
// Each slot of the run routes the candidate waypoints under its own banned arcs
// (maintenance differs per slot), so a single candidate carries one unit-load
// table per run slot, all gated by the same x.  Candidates whose waypoints
// cannot be routed on some run slot are sticky-illegal and dropped, matching the
// design-doc rule that columns that do not survive the copy are unused.

// BuildSticky builds the candidate pool of one sticky round.
//
// hots are the target (slot, arc) cells; only those whose slot lies within
// [seed, seed+span] are attacked this round (a decision at seed cannot move a
// slot outside its copy horizon).  A demand is included when, on some run slot,
// its current routing loads one of those hot arcs, so the copy can relieve a
// hot cell even when the demand is not itself hot on the seed slot (the twin
// case: change the seed slot and let the copy carry the fix to the next slot).
func (gen *Generator) BuildSticky(seed int, hots []HotCell, span int) (*Pool, error) {
	if gen.MaxCandPerDemand <= 0 {
		gen.MaxCandPerDemand = 8
	}
	if gen.FracEps <= 0 {
		gen.FracEps = 1e-9
	}
	if gen.Wp2Cap <= 0 {
		gen.Wp2Cap = defaultWp2Cap
	}
	if span < 1 {
		span = 1
	}
	inst, g, sn := gen.Inst, gen.G, gen.Snap
	m, T := g.M, inst.NSlots
	if seed < 0 || seed >= T {
		return nil, fmt.Errorf("mip: BuildSticky seed slot %d out of range", seed)
	}
	endCap := seed + span
	if endCap > T-1 {
		endCap = T - 1
	}
	hotBySlot := map[int][]int{}
	haveTarget := false
	for _, h := range hots {
		if h.Slot < seed || h.Slot > endCap {
			continue
		}
		hotBySlot[h.Slot] = append(hotBySlot[h.Slot], h.Arc)
		haveTarget = true
	}
	if !haveTarget {
		return nil, nil
	}

	keyOf := func(w []int) string {
		b := make([]byte, 0, len(w)*4)
		for _, x := range w {
			b = append(b, byte(x>>8), byte(x))
		}
		return string(b)
	}

	// Pass 1: decide included demands and each one's sticky run, so the pool
	// universe (the union of all runs) is known before any Load slice is built.
	type demand struct {
		d       int
		run     []int             // slots the decision writes, seed..runEnd
		curLoad map[int][]float64 // incumbent volume-scaled load per run slot
		vol     map[int]float64
		maxEnd  int
	}
	var demands []demand
	maxEnd := seed
	for d := 0; d < inst.NDemands(); d++ {
		if gen.expired() {
			return nil, nil // ran out of wall clock: caller skips this round
		}
		dem := &inst.Demands[d]
		if dem.Volume[seed] == 0.0 {
			continue
		}
		// The copy is an override: this round's seed decision is the newest
		// action and supersedes whatever a previous round left on the slots it
		// reaches (that was an older decision).  So the run extends over every
		// following slot that still carries positive volume, regardless of the
		// incumbent waypoint list, stopping only at the span cap / horizon.
		// Zero-volume slots are not skipped: a waypoint written on a slot that
		// carries no traffic can only burn budget on the boundary transitions,
		// so the run halts at the first one.
		end := seed
		for end < endCap {
			nxt := end + 1
			if dem.Volume[nxt] == 0.0 {
				break
			}
			end = nxt
		}
		// The demand must actually be able to relieve a target hot cell on some
		// run slot to justify touching its routing.
		active := false
		for s := seed; s <= end; s++ {
			targets := hotBySlot[s]
			if len(targets) == 0 {
				continue
			}
			u, err := sn.UnitRoute(d, s, sn.GetWaypoints(d, s))
			if err != nil {
				active = false
				break
			}
			cl := scaleUnit(u, dem.Volume[s])
			for _, ha := range targets {
				if cl[ha] > gen.FracEps {
					active = true
					break
				}
			}
			if active {
				break
			}
		}
		if !active {
			continue
		}
		dd := demand{d: d, curLoad: map[int][]float64{}, vol: map[int]float64{}}
		for s := seed; s <= end; s++ {
			u, err := sn.UnitRoute(d, s, sn.GetWaypoints(d, s))
			if err != nil {
				active = false
				break
			}
			dd.curLoad[s] = scaleUnit(u, dem.Volume[s])
			dd.vol[s] = dem.Volume[s]
			dd.run = append(dd.run, s)
		}
		if !active {
			continue
		}
		dd.maxEnd = end
		demands = append(demands, dd)
		if end > maxEnd {
			maxEnd = end
		}
	}
	if len(demands) == 0 {
		return nil, nil
	}

	// Universe = every slot from seed up to the furthest run end.
	slots := make([]int, 0, maxEnd-seed+1)
	for s := seed; s <= maxEnd; s++ {
		slots = append(slots, s)
	}
	posOf := make(map[int]int, len(slots))
	for i, s := range slots {
		posOf[s] = i
	}
	p := &Pool{inst: inst, g: g, snap: sn, m: m, T: T, Slots: slots}

	var nodes []int
	for w := 0; w < inst.NNodes(); w++ {
		nodes = append(nodes, w)
	}

	// blk copies the demand's incumbent contribution at slot s into the
	// candidate load block (zero when the slot is outside this demand's run, so
	// unmoved contributions produce a zero delta against candidate 0).
	blk := func(s int, into []float64, cur map[int][]float64) []float64 {
		base := posOf[s] * m
		if cl := cur[s]; cl != nil {
			copy(into[base:base+m], cl)
		}
		return into[base : base+m]
	}

	for _, dd := range demands {
		d := dd.d
		dem := &inst.Demands[d]

		mkCur := func() []float64 {
			out := make([]float64, len(slots)*m)
			for _, s := range slots {
				blk(s, out, dd.curLoad)
			}
			return out
		}
		cands := []Candidate{{
			Move: make([]bool, len(slots)),
			Load: mkCur(),
		}}

		// best relief of a candidate across every target hot cell on the run.
		relief := func(unitBySlot map[int][]float64) float64 {
			best := -1.0
			for s, targets := range hotBySlot {
				if s < seed || s > dd.maxEnd {
					continue
				}
				u, ok := unitBySlot[s]
				if !ok {
					continue
				}
				cl := dd.curLoad[s]
				for _, ha := range targets {
					if r := cl[ha] - dd.vol[s]*u[ha]; r > best {
						best = r
					}
				}
			}
			return best
		}

		appendMove := func(wps []int, unitBySlot map[int][]float64) {
			cand := Candidate{
				Wps:  append([]int(nil), wps...),
				Move: make([]bool, len(slots)),
				Load: make([]float64, len(slots)*m),
			}
			for _, s := range dd.run {
				cand.Move[posOf[s]] = true
			}
			for _, s := range slots {
				blk(s, cand.Load, dd.curLoad)
			}
			for _, s := range dd.run {
				u := unitBySlot[s]
				if u == nil {
					continue
				}
				copy(cand.Load[posOf[s]*m:posOf[s]*m+m], scaleUnit(u, dd.vol[s]))
			}
			cands = append(cands, cand)
		}

		// Route a waypoint list on every run slot under that slot's own banned
		// arcs.  A candidate that fails to route on any run slot is
		// sticky-illegal and is dropped entirely (design doc: not used).
		routeAll := func(wps []int) (map[int][]float64, bool) {
			out := map[int][]float64{}
			for _, s := range dd.run {
				u, err := sn.UnitRoute(d, s, wps)
				if err != nil {
					return nil, false
				}
				out[s] = u
			}
			return out, true
		}

		// Targeted candidate generation (internal/cand): the pool of nodes is
		// the demand's hop ball rather than every node in the network.  The
		// family's slots are the whole sticky run, so a candidate must survive
		// the copy on every run slot to be usable -- the same rule routeAll
		// applies below.
		if gen.candOn() {
			reliefAt := func(slot int, u []float64) float64 {
				targets := hotBySlot[slot]
				cl := dd.curLoad[slot]
				if len(targets) == 0 || cl == nil || u == nil {
					return -1
				}
				best := -1.0
				for _, ha := range targets {
					if r := cl[ha] - dd.vol[slot]*u[ha]; r > best {
						best = r
					}
				}
				return best
			}
			hm := map[int][]int{}
			for _, s := range dd.run {
				if len(hotBySlot[s]) > 0 {
					hm[s] = hotBySlot[s]
				}
			}
			alts, err := cand.Build(gen.Snap, gen.CandIX, gen.candHops(), g, inst, cand.Family{
				D:      d,
				Slots:  dd.run,
				Hots:   hm,
				Relief: reliefAt,
			}, gen.Cand)
			if err != nil {
				return nil, fmt.Errorf("sticky demand %d run %v: %w", d, dd.run, err)
			}
			for _, a := range alts {
				appendMove(a.Wps, a.Units)
			}
			if len(cands) > 1 {
				p.Pairs = append(p.Pairs, Pair{D: d, Cand: cands})
			}
			continue
		}

		// Per-demand node set excludes its own source/target.
		nds := make([]int, 0, len(nodes))
		for _, w := range nodes {
			if w == dem.Source || w == dem.Target {
				continue
			}
			nds = append(nds, w)
		}

		type alt struct {
			wps   []int
			units map[int][]float64
			rel   float64
		}
		var alts []alt
		seen := map[string]bool{}
		try := func(wps []int) {
			if len(wps) == 0 {
				return
			}
			k := keyOf(wps)
			if seen[k] {
				return
			}
			seen[k] = true
			units, ok := routeAll(wps)
			if !ok {
				return
			}
			if rel := relief(units); rel > gen.FracEps {
				alts = append(alts, alt{wps: wps, units: units, rel: rel})
			}
		}
		for _, w := range nds {
			try([]int{w})
		}
		pairs := len(nds) * (len(nds) - 1) / 2
		step := 1
		if pairs > gen.Wp2Cap {
			step = (pairs + gen.Wp2Cap - 1) / gen.Wp2Cap
		}
		cnt := 0
		for i := 0; i < len(nds); i++ {
			for j := i + 1; j < len(nds); j++ {
				cnt++
				if step > 1 && cnt%step != 0 {
					continue
				}
				try([]int{nds[i], nds[j]})
			}
		}
		sort.Slice(alts, func(i, j int) bool { return alts[i].rel > alts[j].rel })
		if len(alts) > gen.MaxCandPerDemand {
			alts = alts[:gen.MaxCandPerDemand]
		}
		for _, a := range alts {
			appendMove(a.wps, a.units)
		}
		if len(cands) > 1 {
			p.Pairs = append(p.Pairs, Pair{D: d, Cand: cands})
		}
	}
	return p, nil
}
