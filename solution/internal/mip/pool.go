// Package mip implements the decompose-round of the V1.0 solver: it extracts
// the demands that load a set of hot (slot, arc) cells, generates alternative
// waypoint routings that relieve them, and solves a small Gurobi MIP that picks
// one alternative per demand to minimise the global truncated-lex saturation
// vector, subject to the Hamming budget.
//
// A candidate is an *atomic move* for one demand: it writes one waypoint list
// to the slots of that demand's sticky run, so the run's inter-slot Hamming
// distance stays 0 (pool_sticky.go has the run semantics).  Because every
// demand is moved by at most one candidate per round, the transition Hamming
// cost of each candidate is a precomputed scalar against the *current*
// other-slot routings, so the budget rows stay linear and exact (no bilinear
// cross-slot coupling).
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
	// Radiation lists, in (slot, arc) form, the cells this pool's candidates
	// would put *more* load on than the incumbent does -- where a detour lands.
	// It is the halo's seed set: the demands sitting on those arcs get crowded
	// out by the relief this pool is about to buy, and unfreezing them in the
	// same MIP is what lets the round find a landing spot instead of pushing the
	// hot corner into the next corner.  Sorted by descending total gained load,
	// then (slot, arc), so a truncated halo is deterministic.
	Radiation []HotCell
}

// Cover is the fraction of the network's arcs that at least one candidate
// column of this pool traverses: distinct arcs used / total arcs (design doc
// §15).  It is how broad the round's search was, and the caller turns it into
// the confidence increment miss_delta = cover**2 + 0.1 for a round the MIP proved
// optimal but that still could not drop the first bit: a wide search that
// failed is much stronger evidence that the seed is immovable than a narrow
// one.  The panel of "no move" columns contributes nothing -- Move is false on
// them, so they are not counted.
//
// Cost is O(candidates x moved slots x m); the MIP round it feeds costs orders
// of magnitude more, so it is computed once per round rather than cached.
func (p *Pool) Cover() float64 {
	if p.m <= 0 || len(p.Pairs) == 0 {
		return 0
	}
	seen := make([]bool, p.m)
	n := 0
	for i := range p.Pairs {
		for j := range p.Pairs[i].Cand {
			c := &p.Pairs[i].Cand[j]
			for si, moved := range c.Move {
				if !moved {
					continue
				}
				base := si * p.m
				for a := 0; a < p.m; a++ {
					if !seen[a] && c.Load[base+a] > 0 {
						seen[a] = true
						n++
					}
				}
			}
		}
	}
	return float64(n) / float64(p.m)
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

	// ForceSlots pins the pool's slot universe instead of deriving it from the
	// hot cells.  The halo needs this: its hot cells are the radiation cells of
	// an already-built pool, and the two pools get merged into one MIP, whose
	// candidate Load blocks are addressed by position in Pool.Slots.  Deriving a
	// second, narrower universe for the halo would give the merged pairs
	// incompatible block layouts.
	ForceSlots []int

	// AdmitProb, when strictly between 0 and 1, gates every demand on a roll:
	// the demand enters the pool only if its draw is below the probability.  The
	// halo uses this because it is a gamble, not a stable gain -- it unfreezes
	// demands that were crowded out by this round's own detours, and unfreezing
	// them wholesale is as likely to trade one saturated corner for another as to
	// find a landing spot.  Sampling the demand set makes the halo a perturbation
	// of the base pool rather than a deterministic superset of it.  0 or 1
	// disables the gate, which is what the base build wants.
	//
	// The draw is a hash of (AdmitSeed, AdmitRound, demand), not a value pulled
	// from a shared stream, so demand d's answer does not depend on how many
	// demands were tested before it and the pool is reproducible from the seed
	// alone (see admits).
	AdmitProb  float64
	AdmitSeed  int64
	AdmitRound int
}

// admits reports whether demand d enters this pool.  A gate that is off (prob
// <= 0) admits everything that reaches the caller; a probability of 1 admits
// everything as well, so the roll only ever *removes* demands.
func (gen *Generator) admits(d int) bool {
	if gen.AdmitProb >= 1 {
		return true
	}
	if gen.AdmitProb <= 0 {
		return true // no gate configured: the base build admits on its own tests
	}
	x := uint64(gen.AdmitSeed)*0x9E3779B97F4A7C15 ^
		uint64(gen.AdmitRound)*0xBF58476D1CE4E5B9 ^
		uint64(d)*0x94D049BB133111EB
	x ^= x >> 30
	x *= 0xBF58476D1CE4E5B9
	x ^= x >> 27
	x *= 0x94D049BB133111EB
	x ^= x >> 31
	return float64(x>>11)/float64(uint64(1)<<53) < gen.AdmitProb
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

// ExpandHalo merges the halo into pool and returns it.
//
// The problem it addresses: a round only ever unfreezes the demands that
// currently load a hot cell (see the activity test in BuildSticky).  But a detour
// does not delete load, it moves it -- every candidate that relieves a hot cell
// puts that traffic onto some other arc, and the demands already sitting on
// those arcs are then squeezed by a decision they had no say in.  The MIP cannot
// see that coming, because those demands are not in the pool.  So the round
// relieves the packed corner by packing the next one.
//
// The halo is the second pool built against pool.Radiation -- the cells the
// round's own candidates newly load -- merged into the first.  Its demands get
// candidates that relieve the radiation cells, so the MIP can move them out of
// the way in the same solve, and the shared z keeps it from trading one hot cell
// for another.
//
// The size is bounded on both axes by mult x the base pool's demand count, which
// the caller passes as the "non-halo budget": at most that many radiation cells
// are targeted, and at most that many new demands are admitted.  Radiation is
// already ordered by descending gained load, so a truncation keeps the biggest
// landing spots and is deterministic.
//
// Inside that bound each demand is admitted on its own roll at probability prob
// (see Generator.AdmitProb): the halo is a perturbation of the base pool, so it
// samples the crowded demands instead of deterministically taking all of them.
// prob >= 1 restores the take-everything behaviour.
func (gen *Generator) ExpandHalo(pool *Pool, seed, span, round, mult int, prob float64) (*Pool, error) {
	if pool == nil || mult <= 0 || len(pool.Radiation) == 0 {
		return pool, nil
	}
	// The halo run is clipped to the base pool's universe (ForceSlots) and to the
	// base round's own copy horizon, so both pools address the same Load blocks
	// and the merged pairs mean the same thing.
	if len(pool.Slots) == 0 {
		return nil, fmt.Errorf("mip: ExpandHalo needs a pool with a fixed slot universe")
	}
	budget := len(pool.Pairs)
	if budget == 0 {
		return pool, nil
	}
	limit := mult * budget
	rad := pool.Radiation
	if len(rad) > limit {
		rad = rad[:limit]
	}

	hg := *gen
	hg.ForceSlots = pool.Slots // the merged pairs must share one block layout
	// Each demand enters the halo on its own draw, so the halo is a random
	// sample of the crowded demands rather than all of them (see admits).
	hg.AdmitProb = prob
	hg.AdmitRound = round
	hp, err := hg.BuildSticky(seed, rad, span)
	if err != nil {
		return nil, err
	}
	if hp == nil || len(hp.Pairs) == 0 {
		return pool, nil
	}

	idx := make(map[int]int, len(pool.Pairs))
	for i := range pool.Pairs {
		idx[pool.Pairs[i].D] = i
	}
	added := 0
	for _, hp2 := range hp.Pairs {
		if i, ok := idx[hp2.D]; ok {
			pool.Pairs[i] = mergePair(pool.Pairs[i], hp2)
			continue
		}
		if added >= limit {
			break
		}
		idx[hp2.D] = len(pool.Pairs)
		pool.Pairs = append(pool.Pairs, hp2)
		added++
	}
	// The MIP indexes pairs by demand order only through its own base[] slice,
	// so this is cosmetic -- but it makes two runs' pools print identically.
	sort.Slice(pool.Pairs, func(i, j int) bool { return pool.Pairs[i].D < pool.Pairs[j].D })
	return pool, nil
}

// mergePair unions b's moves into a, keeping a's candidate 0 (the "current
// routing" reference, which buildMode reads as the pair's incumbent).  Identity
// is (waypoints, move mask): the same waypoint list offered for different slots
// is two different moves.
func mergePair(a, b Pair) Pair {
	seen := make(map[string]bool, len(a.Cand))
	for _, c := range a.Cand {
		seen[candKey(c)] = true
	}
	for k, c := range b.Cand {
		if k == 0 {
			continue // a's candidate 0 is the reference and stays where it is
		}
		key := candKey(c)
		if seen[key] {
			continue
		}
		seen[key] = true
		a.Cand = append(a.Cand, c)
	}
	return a
}

// candKey is the identity of a candidate for dedup: its waypoints plus which
// slots it moves.
func candKey(c Candidate) string {
	b := make([]byte, 0, 2*len(c.Wps)+len(c.Move))
	for _, x := range c.Wps {
		b = append(b, byte(x>>8), byte(x))
	}
	b = append(b, 0xff)
	for _, m := range c.Move {
		if m {
			b = append(b, 1)
		} else {
			b = append(b, 0)
		}
	}
	return string(b)
}

func scaleUnit(unit []float64, vol float64) []float64 {
	out := make([]float64, len(unit))
	for i := range unit {
		out[i] = vol * unit[i]
	}
	return out
}
