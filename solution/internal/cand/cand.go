// Package cand generates the candidate waypoints of one decompose round by
// *targeted* structural search instead of brute-force enumeration of the whole
// node set.
//
// The legacy generator treats every node except the demand's own source and
// target as a possible waypoint: 1-waypoint generation costs O(n) routings and
// 2-waypoint generation is C(n-2,2) pairs, stride-sampled once it exceeds
// wp2Cap.  Neither the enumeration nor the sampling knows anything about the
// arc the round is trying to relieve, so the overwhelming majority of those
// evaluations are provably useless (they relieve nothing and are dropped after
// being fully routed).
//
// Three strategies replace it.  Each first builds a *hop ball* around the
// demand's shortest path -- the nodes whose detour s->w->tgt costs at most a
// few extra hops over the direct s->tgt -- and then ranks and prunes that ball
// differently:
//
//	hot_center  rank by undirected hop distance to the two endpoints of the
//	            round's hot arc; keep only nodes that provably offload a hot
//	            arc as pairing material, so 2-waypoint candidates are built
//	            from evidence instead of geometry.  This is what makes larger
//	            detours affordable: pool stays at PoolCap (24) instead of n-2.
//	od_scan     the same ball ranked by pure extra hops, with no hot-arc
//	            knowledge at all.  Its job is to keep the long detours that
//	            hot_center's proximity ranking would crowd out of a capped pool
//	            -- exactly the shapes that balance the *tail* of the load
//	            vector, which is the solver's current weak spot.
//	bottleneck  find which of the round's hot arcs the demand actually crosses
//	            on its current shortest path, ban each one in turn, take the
//	            intermediate nodes of the detour, and cross the detour node sets
//	            of two *different* hot arcs so one candidate relieves both.
//
// The three pools are merged round-robin (so no single strategy can crowd the
// others out of a capped pool), deduplicated by *load signature*, capped per
// shape with two independent budgets, and topped up with ~5% off-hot random
// nodes for exploration.
//
// Why the dedup key is the load signature and not the waypoint list: the MIP
// never sees waypoints, only the resulting unit-load vectors used as column
// coefficients.  Two different waypoint lists that produce the same load are
// the same column in the MIP -- keeping both only makes the model bigger.
//
// Two invariants this package must not break (design doc §9):
//
//   - banning an arc is only ever a way to *discover* nodes.  No candidate
//     carries its ban context: every candidate's load vector is re-routed from
//     scratch by the caller against the real deployed semantics (no ban but the
//     scenario's own blocked arcs).  Caching the detour path itself would make
//     the MIP believe it unloaded an arc that the deployed routing still
//     crosses -- a silent wrong answer.
//   - no floating-point tolerance is introduced anywhere; the tight-arc test
//     uses exact equality, like the ECMP code (CLAUDE.md red line).
package cand

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"sort"

	"tasr/internal/graph"
	"tasr/internal/model"
)

// Router is the routing primitive the strategies rank and filter with.
// *snap.Snap implements it.  UnitRoute returns the dense unit-load vector of
// demand d at slot t routed through wps (an empty wps means the plain shortest
// path); err is non-nil when a segment is disconnected under that slot's
// blocked arcs.  GetWaypoints returns the demand's incumbent waypoint list.
type Router interface {
	UnitRoute(d, t int, wps []int) ([]float64, error)
	GetWaypoints(d, t int) []int
}

// Hops is the hop-count query surface the strategies need.  *hops.Cache
// implements it, and is the one that should be passed in a real run: it retains
// each (banned set, origin) BFS so repeated queries across the demands of a
// round -- and across rounds -- cost a map lookup instead of a BFS.
//
// The three lookups are deliberately the slot-addressed form: every caller
// knows the slot, and the cache is what resolves it to the interned banned set.
type Hops interface {
	ForwardAt(t, src int) []int  // hops src -> w
	ReverseAt(t, dst int) []int  // hops w -> dst
	UndirectedAt(t, v int) []int // hops v <-> w (ranking only)
}

// IndexHops adapts a *graph.Index to the Hops surface.  It is the fallback for
// callers that have no hops.Cache: results still land in the index's LRU, but
// that LRU is shared with the ECMP atom cache and therefore evicts, so a hop
// array needed again next round can be recomputed.  Prefer hops.Cache, which
// retains hop arrays unconditionally and interns the banned sets.
type IndexHops struct{ IX *graph.Index }

func (h IndexHops) ForwardAt(t, src int) []int  { return h.IX.HopsFrom(t, src) }
func (h IndexHops) ReverseAt(t, dst int) []int  { return h.IX.HopsTo(t, dst) }
func (h IndexHops) UndirectedAt(t, v int) []int { return h.IX.HopsUnd(t, v) }

// Strategy names, used both as Options.Mode values and as candidate tags.
const (
	ModeMix        = "mix"
	ModeHotCenter  = "hot_center"
	ModeODScan     = "od_scan"
	ModeBottleneck = "bottleneck"

	// TagGlobal marks a node admitted by the global-relief safety net.  It is a
	// candidate *tag*, never a Mode: the net is switched with Options.GlobalK
	// (independently of Mode) and adds nodes to whatever the mode already built.
	TagGlobal = "global_relief"

	// ModeOff selects the legacy brute-force enumeration.  Build rejects it:
	// the caller branches on it before ever reaching this package.
	ModeOff = "off"
)

// FracEps treats a relief at or below this as zero, matching the generator's
// historical epsilon.  A candidate that does not strictly reduce some hot
// cell's load is not worth a MIP column.
const FracEps = 1e-9

// Options configures pool generation.  Zero values take the defaults below,
// which are the reference implementation's tuned values (design doc §5).
type Options struct {
	Mode    string
	PoolCap int // nodes kept per strategy pool

	// MaxW1 and MaxW2 are *independent* budgets: the 1-waypoint and
	// 2-waypoint lists are capped separately and must not be merged into one
	// list and truncated together.  They bound the candidate columns a demand
	// contributes to the MIP (<= MaxW1+MaxW2), not the pool size.
	MaxW1 int
	MaxW2 int

	MaxBans      int     // bottleneck: max hot arcs banned per demand per slot
	MaxExtraHop  int     // hop-ball growth limit: detour may cost at most this many extra hops
	OffHotPct    float64 // off-hot random top-up, as a fraction of PoolCap
	StretchSlack float64 // metric-stretch band; reserved, not implemented in v1 (always 0)
	Seed         int64   // deterministic seed for the off-hot sample

	// GlobalK enables the global-relief safety net: route every node as a
	// singleton and append the GlobalK that relieve the most, beyond whatever
	// the strategy pools already kept.  0 disables it (the v1 default, so the
	// net is opt-in until it has been measured); it is orthogonal to Mode.
	//
	// It exists because the targeted strategies trade singleton recall for
	// speed, and the microbenchmark measured the price: on setA-04/10/13/16 the
	// pool does not contain the best singleton that brute-force ranking finds
	// (relief capture 0.92-0.98).  Only the singleton side is affected -- the
	// 2-waypoint side is most of the output and has no brute-force counterpart
	// -- so restoring it exactly is cheap and provably closes that gap.
	GlobalK int
}

// withDefaults fills every field the strategies require.  It is deliberately
// size-independent: the default mode does not adapt to the instance node count
// (design doc §12 Q7).  Small instances pay a neutral overhead in exchange for
// better-ranked candidates, and -cand-mode off remains the manual escape hatch.
func (o Options) withDefaults() Options {
	if o.Mode == "" {
		o.Mode = ModeMix
	}
	if o.PoolCap <= 0 {
		o.PoolCap = 24
	}
	if o.MaxW1 <= 0 {
		o.MaxW1 = 24
	}
	if o.MaxW2 <= 0 {
		o.MaxW2 = 32
	}
	if o.MaxBans <= 0 {
		o.MaxBans = 3
	}
	if o.MaxExtraHop <= 0 {
		o.MaxExtraHop = 4
	}
	if o.OffHotPct <= 0 {
		o.OffHotPct = 0.05
	}
	return o
}

// Node is one candidate waypoint of a pool.
type Node struct {
	ID int

	// Hops is hf[w]+ht[w]: the number of arcs on the detour s->w->tgt at the
	// slot the node was admitted for.  It is od_scan's ranking key.
	Hops int

	// Dist is the undirected hop distance from this node to the nearest
	// endpoint of a hot arc.  It is hot_center's ranking key.
	Dist int

	// Tag records the strategy that produced the node; it is for logging and
	// tuning only and never affects a decision.
	Tag string
}

// Family is one generation scope: a demand, the slots a single decision writes
// (one slot for a divergence family, two for a twin, the whole run for sticky),
// the hot arcs to relieve on each of those slots, and how much a routed unit
// vector relieves them.
//
// Relief reports the best improvement a unit vector gives to the incumbent load
// on slot's hot arcs; it returns a negative value when the slot has no target,
// so taking the max over a family's slots ignores it.
type Family struct {
	D      int
	Slots  []int
	Hots   map[int][]int
	Relief func(slot int, unit []float64) float64
}

// Alt is one accepted alternative: its waypoint list, the unit-load vector it
// produces on every family slot, and the relief it gives.
type Alt struct {
	Wps   []int
	Units map[int][]float64
	Rel   float64
	Tag   string
}

// Build renders a family's alternatives.
//
// The returned list is ordered 1-waypoint candidates first (by decreasing
// relief, capped at MaxW1), then 2-waypoint candidates (same, capped at
// MaxW2).  Every returned candidate has been routed for real on every family
// slot and strictly relieves at least one target; no two share a load
// signature.
func Build(rt Router, ix *graph.Index, hp Hops, g *graph.Graph, inst *model.Instance,
	fam Family, opts Options) ([]Alt, error) {
	opts = opts.withDefaults()
	if opts.Mode == ModeOff {
		return nil, fmt.Errorf("cand: Build called with Mode %q; the caller must use the legacy enumeration", ModeOff)
	}
	if len(fam.Slots) == 0 {
		return nil, fmt.Errorf("cand: demand %d: empty family slot list", fam.D)
	}
	if fam.Relief == nil {
		return nil, fmt.Errorf("cand: demand %d: nil Relief", fam.D)
	}
	modes := modesOf(opts.Mode)
	if len(modes) == 0 {
		return nil, fmt.Errorf("cand: unknown mode %q", opts.Mode)
	}

	memo := newRouteMemo(rt, fam.D)

	// 1. One ordered pool per strategy.
	pools := make([]stratPool, 0, len(modes))
	for _, m := range modes {
		switch m {
		case ModeHotCenter:
			pools = append(pools, hotCenter(memo, hp, g, inst, fam, opts))
		case ModeODScan:
			pools = append(pools, odScan(hp, inst, fam, opts))
		case ModeBottleneck:
			pools = append(pools, bottleneck(memo, ix, g, inst, fam, opts))
		}
	}

	// 2. Merge round-robin so a capped pool still represents every strategy,
	// and dedup by node id.
	lists := make([][]Node, 0, len(pools))
	for _, p := range pools {
		lists = append(lists, p.nodes)
	}
	nodes := interleave(lists, opts.PoolCap)
	inPool := make(map[int]bool, len(nodes))
	for _, n := range nodes {
		inPool[n.ID] = true
	}

	// 2b. Global-relief safety net.  Appended *after* the interleave rather than
	// merged into it, so switching it on cannot shrink any strategy's share of
	// the capped pool: the pool is a strict superset of what the mode alone
	// builds, and an A/B that flips GlobalK attributes the difference to the
	// added nodes alone.  These nodes are deliberately not offered as pairing
	// material -- the loss being repaired is on the singleton side, and the pair
	// count is what makes the MIP expensive.
	if opts.GlobalK > 0 {
		for _, n := range globalRelief(memo, inst, fam, opts) {
			if inPool[n.ID] {
				continue
			}
			inPool[n.ID] = true
			nodes = append(nodes, n)
		}
	}

	// 3. Pairing material: the union of the strategies' explicit pairs,
	// restricted to nodes that survived the merge.
	seenPair := map[[2]int]bool{}
	var pairs [][2]int
	for _, p := range pools {
		for _, pr := range p.pairs {
			if !inPool[pr[0]] || !inPool[pr[1]] || seenPair[pr] {
				continue
			}
			seenPair[pr] = true
			pairs = append(pairs, pr)
		}
	}

	// 4. Sequences: the pool's singletons, its explicit pairs, plus the
	// off-hot top-up.
	type seq struct {
		wps []int
		tag string
	}
	seqs := make([]seq, 0, len(nodes)+len(pairs)+1)
	for _, n := range nodes {
		seqs = append(seqs, seq{wps: []int{n.ID}, tag: n.Tag})
	}
	for _, pr := range pairs {
		seqs = append(seqs, seq{wps: []int{pr[0], pr[1]}, tag: opts.Mode})
	}
	for _, w := range offhotSample(inst, fam, inPool, opts) {
		seqs = append(seqs, seq{wps: []int{w}, tag: "offhot"})
	}

	// 5. Route each sequence once, keep the ones that really relieve a target,
	// and drop duplicates by load signature.
	seenSig := map[string]bool{}
	var w1, w2 []Alt
	for _, sq := range seqs {
		units, ok := memo.all(fam.Slots, sq.wps)
		if !ok {
			continue // disconnected on some family slot: not a usable candidate
		}
		rel := reliefOf(fam, units)
		if rel <= FracEps {
			continue
		}
		sig := sigOf(fam.Slots, units)
		if seenSig[sig] {
			continue
		}
		seenSig[sig] = true
		a := Alt{Wps: sq.wps, Units: units, Rel: rel, Tag: sq.tag}
		if len(sq.wps) == 1 {
			w1 = append(w1, a)
		} else {
			w2 = append(w2, a)
		}
	}

	// 6. Two independent budgets.  Never merge these lists and truncate once:
	// a single shared cap of 12 is exactly what the legacy generator did and
	// what this design retires.
	sortByRelief(w1)
	sortByRelief(w2)
	if len(w1) > opts.MaxW1 {
		w1 = w1[:opts.MaxW1]
	}
	if len(w2) > opts.MaxW2 {
		w2 = w2[:opts.MaxW2]
	}
	return append(w1, w2...), nil
}

// reliefOf is the best relief a candidate gives across the family's slots.  A
// slot with no target contributes a negative value, so it never wins the max.
func reliefOf(fam Family, units map[int][]float64) float64 {
	best := -1.0
	for _, t := range fam.Slots {
		u, ok := units[t]
		if !ok {
			continue
		}
		if r := fam.Relief(t, u); r > best {
			best = r
		}
	}
	return best
}

// sortByRelief orders by decreasing relief; ties keep insertion order (pool
// order), so the result is deterministic.
func sortByRelief(a []Alt) {
	sort.SliceStable(a, func(i, j int) bool { return a[i].Rel > a[j].Rel })
}

// modesOf expands a mode into the strategy list it runs.
func modesOf(mode string) []string {
	switch mode {
	case ModeMix:
		// Order matters: it is the round-robin priority when the merged pool is
		// capped, and hot_center is the primary strategy.
		return []string{ModeHotCenter, ModeODScan, ModeBottleneck}
	case ModeHotCenter, ModeODScan, ModeBottleneck:
		return []string{mode}
	}
	return nil
}

// interleave merges already-ranked node lists round-robin, dropping duplicate
// ids and stopping at cap.  Round-robin rather than concatenate-then-truncate
// is what keeps a capped pool representative: plain concatenation would let the
// first list consume the whole budget.
func interleave(lists [][]Node, cap int) []Node {
	idx := make([]int, len(lists))
	seen := map[int]bool{}
	out := make([]Node, 0, cap)
	for {
		progress := false
		for i, l := range lists {
			if idx[i] >= len(l) {
				continue
			}
			n := l[idx[i]]
			idx[i]++
			progress = true
			if seen[n.ID] {
				continue
			}
			seen[n.ID] = true
			out = append(out, n)
			if len(out) >= cap {
				return out
			}
		}
		if !progress {
			return out
		}
	}
}

// offhotSample picks the ~5% off-hot top-up: nodes that no strategy put in the
// pool, sampled deterministically from Options.Seed so the same invocation
// always produces the same pool.  The count is a fraction of the *pool cap*,
// not of the node count -- at n=400 a 5%-of-n sample would be 20 nodes and
// would drown the structured pool it is meant to complement.
func offhotSample(inst *model.Instance, fam Family, inPool map[int]bool, opts Options) []int {
	n := int(math.Round(opts.OffHotPct * float64(opts.PoolCap)))
	if n <= 0 {
		return nil
	}
	dem := &inst.Demands[fam.D]
	domain := make([]int, 0, inst.NNodes())
	for w := 0; w < inst.NNodes(); w++ {
		if w == dem.Source || w == dem.Target || inPool[w] {
			continue
		}
		domain = append(domain, w)
	}
	if n > len(domain) {
		n = len(domain)
	}
	if n == 0 {
		return nil
	}
	rng := rand.New(rand.NewSource(mixSeed(opts.Seed, fam.D, fam.Slots[0])))
	rng.Shuffle(len(domain), func(i, j int) { domain[i], domain[j] = domain[j], domain[i] })
	return domain[:n]
}

// mixSeed derives a per-(demand, slot) seed so different families sample
// independently while the whole run stays reproducible from Options.Seed.
func mixSeed(seed int64, d, t int) int64 {
	h := uint64(seed)*0x9E3779B97F4A7C15 +
		uint64(d)*0xBF58476D1CE4E5B9 +
		uint64(t)*0x94D049BB133111EB
	h ^= h >> 31
	return int64(h)
}

// sigOf fingerprints a candidate's load: the non-zero arcs and their exact bit
// patterns, per family slot.  Exact bits (not a rounded or tolerance-compared
// key) because the MIP coefficients are these very floats: two candidates that
// differ in any ulp are genuinely different columns.
func sigOf(slots []int, units map[int][]float64) string {
	var b []byte
	var tmp [8]byte
	for _, t := range slots {
		b = append(b, byte(t>>8), byte(t))
		u := units[t]
		for a, v := range u {
			if v == 0.0 {
				continue
			}
			b = append(b, byte(a>>8), byte(a))
			binary.LittleEndian.PutUint64(tmp[:], math.Float64bits(v))
			b = append(b, tmp[:]...)
		}
		b = append(b, '|')
	}
	return string(b)
}

// routeMemo caches the unit vector of one (demand, slot, waypoint list) so a
// node routed while the pool is being built (hot_center's offload test) is not
// routed a second time when its sequence is emitted.  This is also the fix for
// the legacy generator's double routing: it routed every surviving candidate
// once inside the relief closure and again when materialising the move.
type routeMemo struct {
	rt    Router
	d     int
	units map[string][]float64
	fail  map[string]bool
}

func newRouteMemo(rt Router, d int) *routeMemo {
	return &routeMemo{rt: rt, d: d, units: map[string][]float64{}, fail: map[string]bool{}}
}

func (r *routeMemo) wps(t int) []int { return r.rt.GetWaypoints(r.d, t) }

func memoKey(t int, wps []int) string {
	k := []byte{byte(t >> 8), byte(t)}
	for _, x := range wps {
		k = append(k, byte(x>>8), byte(x))
	}
	return string(k)
}

// at routes wps at slot t, memoised.  ok is false when the segment chain is
// disconnected under that slot's blocked arcs.
func (r *routeMemo) at(t int, wps []int) ([]float64, bool) {
	k := memoKey(t, wps)
	if u, ok := r.units[k]; ok {
		return u, true
	}
	if r.fail[k] {
		return nil, false
	}
	u, err := r.rt.UnitRoute(r.d, t, wps)
	if err != nil {
		// Sticky-illegal candidate: disconnected on this slot.  Recorded, not
		// fatal -- a candidate that cannot survive the copy is simply unused.
		r.fail[k] = true
		return nil, false
	}
	r.units[k] = u
	return u, true
}

// all routes wps on every family slot, or reports false if any of them fails.
func (r *routeMemo) all(slots []int, wps []int) (map[int][]float64, bool) {
	out := make(map[int][]float64, len(slots))
	for _, t := range slots {
		u, ok := r.at(t, wps)
		if !ok {
			return nil, false
		}
		out[t] = u
	}
	return out, true
}
