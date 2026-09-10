// Package mip implements the decompose-round of the V1.0 solver: it extracts
// the demands that load a set of hot (slot, arc) cells, generates alternative
// waypoint routings that relieve them, and solves a small Gurobi MIP that picks
// one alternative per demand to minimise the global truncated-lex saturation
// vector, subject to the Hamming budget.
//
// A candidate is an *atomic move* for one demand: it writes one waypoint list
// to a chosen subset of the pool's time slots (a single slot, or a twin pair of
// adjacent slots that both get the same waypoints so the inter-slot Hamming
// distance stays 0).  Because every demand is moved by at most one candidate
// per round, the transition Hamming cost of each candidate is a precomputed
// scalar against the *current* other-slot routings, so the budget rows stay
// linear and exact (no bilinear cross-slot coupling).
//
// The MIP works over precomputed ECMP loads, so it is a pure 0-1 selection
// problem with no embedded shortest paths.  Candidates that leave a slot
// unmoved keep that slot's current contribution (delta 0).
package mip

import (
	"fmt"
	"sort"
	"time"

	"tasr/internal/cand"
	"tasr/internal/graph"
	"tasr/internal/model"
	"tasr/internal/snap"
)

// HotCell is one (time slot, arc) saturation cell to relieve.
type HotCell struct {
	Slot int
	Arc  int
}

// Candidate is one atomic waypoint move for a (demand, pool).  Wps is written
// to every slot whose pool position has Move true; Load holds the demand's
// contribution on every pool slot (length m*len(pool.Slots), block per slot),
// using the *new* path on moved slots and the *current* contribution elsewhere
// so unmoved blocks contribute zero delta.
type Candidate struct {
	Wps  []int
	Move []bool
	Load []float64
}

// Pair groups the atomic alternatives of one demand.
type Pair struct {
	D    int
	Cand []Candidate // index 0 is always the current routing (Move all false)
}

// Pool is the candidate pool over one universe of time slots.
type Pool struct {
	inst  *model.Instance
	g     *graph.Graph
	snap  *snap.Snap
	m     int
	T     int
	Slots []int // time slots in this pool's universe (sorted ascending)
	Pairs []Pair
}

// Generator builds a pool for the given hot cells.
type Generator struct {
	Inst             *model.Instance
	G                *graph.Graph
	Snap             *snap.Snap
	MaxCandPerDemand int     // legacy path only: cap of alternatives kept per demand per family
	FracEps          float64 // treat load below this as zero on a hot cell
	Wp2Cap           int     // legacy path only: max 2-waypoint (w1,w2) evaluations per family

	// Cand selects the targeted candidate generator (internal/cand).  CandIX is
	// the graph index it queries, shared with the ECMP atom cache; passing it
	// is what turns the generator on.  Leaving CandIX nil keeps the legacy
	// brute-force enumeration, which is also what Cand.Mode == cand.ModeOff
	// requests explicitly (used for A/B and bit-for-bit regression).
	Cand   cand.Options
	CandIX *graph.Index

	// CandHops is the hop-count cache the strategies query (internal/hops).
	// It is optional: nil makes every hop query fall back to a fresh BFS via
	// the graph index, which is the "without cache" arm of the benchmark.
	CandHops cand.Hops

	// Deadline, when non-zero, aborts a pool build that runs past it: the
	// builder returns a nil pool and the caller skips the round.  Pool build is
	// the one phase with no natural cost bound (it does a real routing per
	// demand per strategy), so it is where a runaway would otherwise eat the
	// whole wall-clock budget.  Zero means no cap (used by tests).
	Deadline time.Time
}

// expired reports whether the generator's deadline has passed.
func (gen *Generator) expired() bool {
	return !gen.Deadline.IsZero() && time.Now().After(gen.Deadline)
}

// candOn reports whether the targeted generator replaces the legacy
// enumeration.  CandIX is the switch: without a graph index the strategies have
// nothing to query, so a Generator built without one behaves exactly as before.
func (gen *Generator) candOn() bool {
	return gen.CandIX != nil && gen.Cand.Mode != cand.ModeOff
}

// candHops resolves the hop-query surface the strategies use: the dedicated
// hop cache when the caller supplied one, otherwise the graph index's LRU.
func (gen *Generator) candHops() cand.Hops {
	if gen.CandHops != nil {
		return gen.CandHops
	}
	return cand.IndexHops{IX: gen.CandIX}
}

// defaultWp2Cap bounds the 2-waypoint enumeration.  The full search space is
// C(n-2,2) waypoint pairs per demand per family, which is fine up to roughly a
// hundred nodes but explodes quadratically on the largest setA instances
// (n up to 400 -> ~80k pairs per family, times the many demands that load a
// saturated hot arc).  When the pair count exceeds the cap the scan is
// stride-sampled evenly over the colexicographic order so the pool still sees a
// representative spread of detours at a bounded cost.  Pairs <= cap scan
// identically to an uncapped run, so small instances are unaffected.
const defaultWp2Cap = 8000

// Build returns a single-slot pool relieving one hot arc at slot t.
func (gen *Generator) Build(t, arcA int) (*Pool, error) {
	return gen.BuildCells([]HotCell{{Slot: t, Arc: arcA}})
}

// BuildCells returns the pool of demands whose current routing loads at least
// one hot cell, with single-slot and twin-pair alternatives that strictly
// reduce the load of at least one hot cell.
//
// The pool universe is the hot slots extended by their immediate neighbours so
// that a twin (same waypoints on both endpoints of a transition, distance 0)
// can be offered for the cheapest kind of relief.  Candidates are generated per
// demand and family (each hot slot individually, plus each adjacent pair that
// the demand is active on), deduplicated inside a family, ranked by the largest
// relief they give to any hot cell, and truncated to MaxCandPerDemand each.
func (gen *Generator) BuildCells(hots []HotCell) (*Pool, error) {
	if gen.MaxCandPerDemand <= 0 {
		gen.MaxCandPerDemand = 8
	}
	if gen.FracEps <= 0 {
		gen.FracEps = 1e-9
	}
	if gen.Wp2Cap <= 0 {
		gen.Wp2Cap = defaultWp2Cap
	}
	if len(hots) == 0 {
		return nil, fmt.Errorf("mip: BuildCells with empty hot-cell set")
	}
	inst, g, sn := gen.Inst, gen.G, gen.Snap
	m, T := g.M, inst.NSlots

	// Universe = hot slots plus their immediate transition neighbours.
	univ := map[int]bool{}
	for _, h := range hots {
		univ[h.Slot] = true
		if h.Slot > 0 {
			univ[h.Slot-1] = true
		}
		if h.Slot+1 < T {
			univ[h.Slot+1] = true
		}
	}
	var slots []int
	for s := range univ {
		slots = append(slots, s)
	}
	sort.Ints(slots)
	posOf := make(map[int]int, len(slots))
	for i, s := range slots {
		posOf[s] = i
	}
	hotBySlot := map[int][]int{}
	for _, h := range hots {
		hotBySlot[h.Slot] = append(hotBySlot[h.Slot], h.Arc)
	}

	p := &Pool{inst: inst, g: g, snap: sn, m: m, T: T, Slots: slots}
	keyOf := func(w []int) string {
		b := make([]byte, 0, len(w)*4)
		for _, x := range w {
			b = append(b, byte(x>>8), byte(x))
		}
		return string(b)
	}

	n := inst.NNodes()
	for d := 0; d < inst.NDemands(); d++ {
		if gen.expired() {
			return nil, nil // ran out of wall clock: caller skips this round
		}
		dem := &inst.Demands[d]

		// Current per-slot contribution of this demand inside the universe.
		curWps := map[int][]int{}
		curUnit := map[int][]float64{}
		curLoad := map[int][]float64{}
		vol := map[int]float64{}
		active := false
		for _, s := range slots {
			v := dem.Volume[s]
			if v == 0.0 {
				continue
			}
			w := sn.GetWaypoints(d, s)
			u, err := sn.UnitRoute(d, s, w)
			if err != nil {
				continue // current routing disconnected: skip this demand
			}
			curWps[s] = w
			curUnit[s] = u
			curLoad[s] = scaleUnit(u, v)
			vol[s] = v
			active = true
		}
		if !active {
			continue
		}

		// Which hot cells does this demand load?
		loadsHot := map[int]bool{}
		anyHot := false
		for _, s := range slots {
			cl := curLoad[s]
			if cl == nil {
				continue
			}
			for _, ha := range hotBySlot[s] {
				if cl[ha] > gen.FracEps {
					loadsHot[s] = true
					anyHot = true
					break
				}
			}
		}
		if !anyHot {
			continue
		}

		// Shared helper: base block slice of the demand's current contribution
		// at a slot (nil -> zero vector).  Unmoved blocks alias these slices,
		// which are never mutated, so aliasing is safe.
		blk := func(s int, into []float64) []float64 {
			base := posOf[s] * m
			if cl := curLoad[s]; cl != nil {
				copy(into[base:base+m], cl)
			}
			return into[base : base+m]
		}

		mkLoad := func(moved []int) []float64 {
			out := make([]float64, len(slots)*m)
			for _, s := range slots {
				blk(s, out)
			}
			return out
		}

		cands := []Candidate{{
			Move: make([]bool, len(slots)),
			Load: mkLoad(nil),
		}}
		// The whole network node set is the waypoint search space.
		var nodes []int
		for w := 0; w < n; w++ {
			if w == dem.Source || w == dem.Target {
				continue
			}
			nodes = append(nodes, w)
		}

		appendMove := func(movedSlots []int, wps []int, unitBySlot map[int][]float64) {
			cand := Candidate{
				Wps:  append([]int(nil), wps...),
				Move: make([]bool, len(slots)),
				Load: mkLoad(nil),
			}
			for _, s := range movedSlots {
				cand.Move[posOf[s]] = true
				u := unitBySlot[s]
				if u == nil {
					continue
				}
				copy(cand.Load[posOf[s]*m:posOf[s]*m+m], scaleUnit(u, vol[s]))
			}
			cands = append(cands, cand)
		}

		// bestRel of a per-slot unit vector against the arcs hot at that slot.
		// A slot the demand is not active on has no incumbent load, so it
		// reports no relief (the cand path may probe slots outside this
		// demand's map).
		relief := func(slot int, unit []float64, targetArcs []int) float64 {
			cl := curLoad[slot]
			if cl == nil || unit == nil {
				return -1
			}
			best := -1.0
			for _, ha := range targetArcs {
				if r := cl[ha] - vol[slot]*unit[ha]; r > best {
					best = r
				}
			}
			return best
		}

		// Enumerate 1-waypoint and 2-waypoint lists and keep those with relief.
		type alt struct {
			wps []int
			rel float64
		}
		enumerate := func(relFor func(wps []int) float64, unitAt func(wps []int) ([]float64, error)) []alt {
			var alts []alt
			seen := map[string]bool{}
			try := func(wps []int) {
				k := keyOf(wps)
				if seen[k] {
					return
				}
				seen[k] = true
				if rel := relFor(wps); rel > gen.FracEps {
					alts = append(alts, alt{wps: wps, rel: rel})
				}
			}
			for _, w := range nodes {
				try([]int{w})
			}
			pairs := len(nodes) * (len(nodes) - 1) / 2
			step := 1
			if pairs > gen.Wp2Cap {
				// Stride-sample so an over-cap scan still covers the pair space
				// evenly instead of exhausting it.  cnt counts every pair, so
				// step spreads the ~Wp2Cap evaluations across all (i,j).
				step = (pairs + gen.Wp2Cap - 1) / gen.Wp2Cap
			}
			cnt := 0
			for i := 0; i < len(nodes); i++ {
				for j := i + 1; j < len(nodes); j++ {
					cnt++
					if step > 1 && cnt%step != 0 {
						continue
					}
					try([]int{nodes[i], nodes[j]})
				}
			}
			sort.Slice(alts, func(i, j int) bool { return alts[i].rel > alts[j].rel })
			if len(alts) > gen.MaxCandPerDemand {
				alts = alts[:gen.MaxCandPerDemand]
			}
			return alts
		}

		// Single-slot families: reroute the slot whose hot cell we relieve.
		for _, s := range slots {
			if !loadsHot[s] {
				continue
			}
			target := hotBySlot[s]
			if gen.candOn() {
				alts, err := cand.Build(gen.Snap, gen.CandIX, gen.candHops(), g, inst, cand.Family{
					D:     d,
					Slots: []int{s},
					Hots:  map[int][]int{s: target},
					Relief: func(slot int, u []float64) float64 {
						return relief(slot, u, hotBySlot[slot])
					},
				}, gen.Cand)
				if err != nil {
					return nil, fmt.Errorf("demand %d slot %d: %w", d, s, err)
				}
				for _, a := range alts {
					appendMove([]int{s}, a.Wps, a.Units)
				}
				continue
			}
			alts := enumerate(
				func(wps []int) float64 {
					u, err := sn.UnitRoute(d, s, wps)
					if err != nil {
						return -1
					}
					return relief(s, u, target)
				},
				func(wps []int) ([]float64, error) { return sn.UnitRoute(d, s, wps) },
			)
			for _, a := range alts {
				u, _ := sn.UnitRoute(d, s, a.wps)
				appendMove([]int{s}, a.wps, map[int][]float64{s: u})
			}
		}

		// Twin families: same waypoints on both endpoints of an adjacent
		// (u, v = u+1) pair inside the universe.  The two slots then have
		// identical node-pair chains, so their mutual Hamming cost stays 0 and
		// the relief is budget-free.
		for _, u := range slots {
			v := u + 1
			if v >= T || !univ[v] {
				continue
			}
			if vol[u] == 0.0 || vol[v] == 0.0 {
				continue
			}
			if !loadsHot[u] && !loadsHot[v] {
				continue
			}
			var target []HotCell
			for _, h := range hots {
				if (h.Slot == u || h.Slot == v) && loadsHot[h.Slot] {
					target = append(target, h)
				}
			}
			if gen.candOn() {
				// Only the slots that actually load a hot cell carry targets; the
				// other endpoint still gets the same waypoints written (that is
				// what makes the pair a twin), it just has nothing to relieve.
				hm := map[int][]int{}
				for _, h := range hots {
					if (h.Slot == u || h.Slot == v) && loadsHot[h.Slot] {
						hm[h.Slot] = append(hm[h.Slot], h.Arc)
					}
				}
				alts, err := cand.Build(gen.Snap, gen.CandIX, gen.candHops(), g, inst, cand.Family{
					D:     d,
					Slots: []int{u, v},
					Hots:  hm,
					Relief: func(slot int, uu []float64) float64 {
						return relief(slot, uu, hm[slot])
					},
				}, gen.Cand)
				if err != nil {
					return nil, fmt.Errorf("demand %d twin %d-%d: %w", d, u, v, err)
				}
				for _, a := range alts {
					appendMove([]int{u, v}, a.Wps, a.Units)
				}
				continue
			}
			alts := enumerate(
				func(wps []int) float64 {
					uA, errA := sn.UnitRoute(d, u, wps)
					uB, errB := sn.UnitRoute(d, v, wps)
					if errA != nil || errB != nil {
						return -1
					}
					best := -1.0
					for _, h := range target {
						cl := curLoad[h.Slot]
						var uu []float64
						if h.Slot == u {
							uu = uA
						} else {
							uu = uB
						}
						if r := cl[h.Arc] - vol[h.Slot]*uu[h.Arc]; r > best {
							best = r
						}
					}
					return best
				},
				func(wps []int) ([]float64, error) {
					return sn.UnitRoute(d, u, wps)
				},
			)
			for _, a := range alts {
				uA, _ := sn.UnitRoute(d, u, a.wps)
				uB, _ := sn.UnitRoute(d, v, a.wps)
				appendMove([]int{u, v}, a.wps, map[int][]float64{u: uA, v: uB})
			}
		}

		if len(cands) <= 1 {
			continue // no 1-wp or 2-wp move relieves any hot cell for this demand
		}
		p.Pairs = append(p.Pairs, Pair{D: d, Cand: cands})
	}
	return p, nil
}

func scaleUnit(unit []float64, vol float64) []float64 {
	out := make([]float64, len(unit))
	for i := range unit {
		out[i] = vol * unit[i]
	}
	return out
}
