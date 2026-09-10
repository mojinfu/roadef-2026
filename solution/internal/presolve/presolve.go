// Package presolve freezes the structurally immovable (slot, arc) cells of a
// solve before the outer loop starts.
//
// Motivation.  The outer loop spends its rounds on the globally hottest cells,
// but a large share of those cells are hot *because they cannot be avoided*:
// every routing of some demand crosses them, whatever waypoints are chosen.
// No round can ever lower them, so each attempt is a wasted Gurobi solve.  The
// scan below turns that into a hard fact -- a per-cell lower bound on the load,
// and, when the bound accounts for the whole observed load, a permanent pin.
//
// Three sufficient criteria, cheapest first:
//
//	class 1 (bridge)     the arc is one direction of a cut edge {u,v} of the
//	                     undirected topology, and some positive-volume demand
//	                     has its endpoints on opposite sides.  Every route
//	                     crosses the cut, hence the edge; if the opposite
//	                     direction is unavailable the load must take this one.
//	class 2 (dominance)  the arc is the *unique* tight arc into its head (sink
//	                     dominator) or out of its tail (source dominator).  A
//	                     waypoint cannot force a path longer than the shortest
//	                     path, so every route through that head/tail uses it.
//	class 3 (exact)      per-demand dodge test over the "tight-arc DAG" of the
//	                     base metric (an arc x -> y is a DAG edge when
//	                     dist(s,y) == dist(s,x) + w(x,y)): a demand dodges the
//	                     cell iff its target stays reachable with the cell's arc
//	                     removed.  Any SR route is a walk in that DAG (every
//	                     shortest-path segment decomposes into tight arcs), so
//	                     class 3 is exact for SR+SP routing and subsumes 1 and 2.
//	                     It costs one distance array per (slot, source), so it
//	                     runs on the hottest cells only, inside the same budget.
//
// Classes 1 and 2 need no assumption about the rest of the solution, so their
// forced sum is a sound lower bound:
//
//	load(t, e) >= sum of the volumes the criterion forces onto e.
//
// That floor holds for *every* routing, which is what makes it useful in the
// search: once a cell's observed load sits on its floor, no round can lower it,
// and when such a cell is the global maximum the first bit of the objective is
// already optimal (Bounds.SatFloor certifies it).
//
// A full pin (lb == ub) needs the opposite side as well: not only must the
// forced demands cover the observed load, *no other demand may ever reach the
// arc*.  Equality with the observed load proves nothing on its own -- it says
// that no other demand is on the arc right now, while a later round could still
// move one there (which is exactly what the MIP's pin check caught).  The extra
// test is the noNewUser scan below: for every other demand of the slot, a
// detour through the arc must be impossible on the base graph.  Pins are
// therefore rare and honest; everything else stays a floor.
//
// Runtime upgrade.  TestKey re-runs the exact judgement for one cell against
// the *current* snapshot, so a cell that only becomes structural after the
// first rounds can still be retired instead of consuming rounds forever.
//
// Everything here is pure with respect to the Graph: no state is attached to
// it, because the graph is a stateless adjacency table shared with the ECMP
// cache.  The pin table lives in Bounds, owned by the solve loop.
//
// Float conventions match internal/ecmp (and the official checker): tightness
// is *exact* equality.  The pin decision compares two sums of the same volumes
// accumulated in different orders, so it uses a small relative tolerance far
// below the 1e-6 the objective truncates to.
//
// Soundness rule -- 宁漏勿错: a missed pin costs one wasted round, a wrong pin
// costs correctness.  Every dubious case therefore falls back to a lower bound
// only, or is dropped entirely.
package presolve

import (
	"bufio"
	"fmt"
	"math"
	"math/bits"
	"os"
	"sort"
	"strconv"
	"time"

	"tasr/internal/graph"
	"tasr/internal/model"
	"tasr/internal/snap"
)

// Key is the cell identifier; an alias of the snapshot key so callers can pass
// their own done/fail maps around without converting.
type Key = snap.Key

// Mode selects how aggressive the presolve is.
type Mode int

const (
	// Off disables the presolve entirely.
	Off Mode = iota
	// Unmovable runs the structural scan plus the budgeted exact pass and wires
	// the resulting pins into the solve loop.  This is the default.
	Unmovable
)

// ParseMode maps a CLI string to a Mode ("" => Unmovable, the documented
// default).
func ParseMode(s string) (Mode, error) {
	switch s {
	case "", "unmovable", "on":
		return Unmovable, nil
	case "off", "none":
		return Off, nil
	default:
		return Off, fmt.Errorf("presolve: unknown mode %q (want unmovable or off)", s)
	}
}

// Options configures Run / TestKey.  TimeLimit is a *global* budget for the
// whole presolve phase: one pass, one call at solve start.  Once exceeded the
// scan stops and the conclusions already drawn stay valid.
type Options struct {
	TimeLimit time.Duration
	Mode      Mode
	// MaxDeep caps how many cells the exact (class 3) pass examines, in
	// decreasing saturation order.  0 means "as many as the time budget allows".
	MaxDeep int
	// TolRel is the relative tolerance of the consistency check.
	TolRel float64
}

func (o Options) tol() float64 {
	if o.TolRel > 0 {
		return o.TolRel
	}
	return 1e-9
}

func (o Options) limit() time.Duration {
	if o.TimeLimit > 0 {
		return o.TimeLimit
	}
	return 15 * time.Second
}

// Bound is the recorded (lb, ub) pair of one cell.  UB is +Inf for a
// lower-bound-only pin (the common case), so LB alone is the floor of the cell.
type Bound struct {
	LB     float64
	UB     float64
	Proven bool // lb == ub: immovable, and no other demand can ever reach it
	Source string
}

// Bounds is the mutable pin table of one solve, owned by the solve loop (never
// by the graph).  Not safe for concurrent use.
type Bounds struct {
	m map[Key]Bound
}

// NewBounds returns an empty pin table.
func NewBounds() *Bounds { return &Bounds{m: map[Key]Bound{}} }

// Pin fixes a cell at a known load (lb == ub).
func (b *Bounds) Pin(t, arc int, load float64) {
	b.PinProven(t, arc, load, load, "pin")
}

// PinLower records a lower bound only: the cell may still change, but never
// below lb.
func (b *Bounds) PinLower(t, arc int, lb float64) {
	b.set(t, arc, Bound{LB: lb, UB: math.Inf(1), Source: "lower"})
}

// PinProven records a fully proven cell (lb == ub, with a provenance tag).
func (b *Bounds) PinProven(t, arc int, lb, ub float64, source string) {
	b.set(t, arc, Bound{LB: lb, UB: ub, Proven: true, Source: source})
}

func (b *Bounds) set(t, arc int, v Bound) {
	if b.m == nil {
		b.m = map[Key]Bound{}
	}
	k := Key{T: t, A: arc}
	if old, ok := b.m[k]; ok {
		// Keep the tightest interval; a provenance upgrade wins ties.
		if old.LB > v.LB {
			v.LB = old.LB
		}
		if old.UB < v.UB {
			v.UB = old.UB
		}
		v.Proven = old.Proven || v.Proven
		if v.Source == "" {
			v.Source = old.Source
		}
	}
	b.m[k] = v
}

// Get returns the recorded bound of one cell.
func (b *Bounds) Get(t, arc int) (Bound, bool) {
	v, ok := b.m[Key{T: t, A: arc}]
	return v, ok
}

// Lower returns the cell's lower bound, or (0, false) when unpinned.
func (b *Bounds) Lower(t, arc int) (float64, bool) {
	v, ok := b.m[Key{T: t, A: arc}]
	if !ok {
		return 0, false
	}
	return v.LB, true
}

// Upper returns the cell's upper bound, or (+Inf, false) when unpinned.
func (b *Bounds) Upper(t, arc int) (float64, bool) {
	v, ok := b.m[Key{T: t, A: arc}]
	if !ok {
		return math.Inf(1), false
	}
	return v.UB, true
}

// ProvenKeys returns the cells with lb == ub, sorted by (T, A) for determinism.
// Those are the cells the solve loop may retire and the MIP may drop from its
// max constraint.
func (b *Bounds) ProvenKeys() []Key {
	out := make([]Key, 0, len(b.m))
	for k, v := range b.m {
		if v.Proven {
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].T != out[j].T {
			return out[i].T < out[j].T
		}
		return out[i].A < out[j].A
	})
	return out
}

// Len returns the number of recorded cells.
func (b *Bounds) Len() int { return len(b.m) }

// FloorMap returns a copy of the recorded lower bounds as a plain map, for the
// MIP builder: every entry is a load the cell can never drop below, so the MIP
// can add a row for it (the objective it reports is then honest instead of
// optimistically low) and the max constraint never has to push below it.
func (b *Bounds) FloorMap() map[Key]float64 {
	out := make(map[Key]float64, len(b.m))
	for k, v := range b.m {
		if v.LB > 0 {
			out[k] = v.LB
		}
	}
	return out
}

// SatFloor returns the highest saturation implied by any recorded lower bound,
// i.e. a valid lower bound on the first bit of the objective.  Every lower
// bound is sound, proven or not, so all of them count.
func (b *Bounds) SatFloor(cap []float64) float64 {
	floor := 0.0
	for k, v := range b.m {
		if k.A < 0 || k.A >= len(cap) || cap[k.A] <= 0 {
			continue
		}
		if s := v.LB / cap[k.A]; s > floor {
			floor = s
		}
	}
	return floor
}

// Pin is one reported pin, for the console report and the TSV dump.
type Pin struct {
	T, Arc  int
	Load    float64
	LB      float64
	Sat     float64
	Source  string
	Proven  bool
	Demands int
}

// Report summarises one presolve run.
type Report struct {
	Pins     []Pin
	LBSat    float64
	Cells    int // positive-load cells seen by the structural pass
	Deep     int // cells re-examined by the exact pass
	Pinned   int
	Proven   int
	Revoked  int // forced sum > observed load: impossible for a sound bound
	TimedOut bool
	Elapsed  time.Duration
}

// forced accumulates, per arc, the volume of the demands a criterion has proven
// must cross it.
//
// One demand may be forced by several criteria at once (a cut arc is often also
// the unique tight arc of its tail), so the accumulator counts each demand at
// most once per arc; without that, the forced sum would double-count and could
// exceed the observed load for a perfectly sound argument.
type forced struct {
	vol    map[int]float64
	dem    map[int]map[int]bool // arc -> demand ids already counted
	ndem   map[int]int
	source map[int]map[string]bool // arc -> contributing criteria
}

func newForced() *forced {
	return &forced{
		vol:    map[int]float64{},
		dem:    map[int]map[int]bool{},
		ndem:   map[int]int{},
		source: map[int]map[string]bool{},
	}
}

func (f *forced) add(arc, d int, vol float64, source string) {
	if arc < 0 || vol <= 0 {
		return
	}
	if f.dem[arc] == nil {
		f.dem[arc] = map[int]bool{}
	}
	if f.dem[arc][d] {
		return
	}
	f.dem[arc][d] = true
	f.vol[arc] += vol
	f.ndem[arc]++
	if f.source[arc] == nil {
		f.source[arc] = map[string]bool{}
	}
	f.source[arc][source] = true
}

// setExact records the result of a criterion that is already exact and
// deduplicated by construction (the class-3 dodge test sums each demand once).
func (f *forced) setExact(arc int, vol float64, ndem int, source string) {
	if arc < 0 || vol <= 0 {
		return
	}
	f.vol[arc] += vol
	f.ndem[arc] += ndem
	if f.source[arc] == nil {
		f.source[arc] = map[string]bool{}
	}
	f.source[arc][source] = true
}

func (f *forced) arcs() []int {
	out := make([]int, 0, len(f.vol))
	for a := range f.vol {
		out = append(out, a)
	}
	sort.Ints(out)
	return out
}

// tag renders the contributing criteria of an arc in a stable order, e.g.
// "bridge+sink-dom".
func (f *forced) tag(arc int) string {
	if len(f.source[arc]) == 0 {
		return ""
	}
	names := make([]string, 0, len(f.source[arc]))
	for s := range f.source[arc] {
		names = append(names, s)
	}
	sort.Strings(names)
	out := names[0]
	for _, s := range names[1:] {
		out += "+" + s
	}
	return out
}

// slotDemands is the per-slot view of the demand set: which demands carry
// volume, grouped by target and by source, plus deterministic orderings.
type slotDemands struct {
	ids      []int
	byTgt    map[int][]int
	bySrc    map[int][]int
	tgtOrder []int
	srcOrder []int
}

func indexDemands(inst *model.Instance) []*slotDemands {
	T := inst.NSlots
	out := make([]*slotDemands, T)
	for t := 0; t < T; t++ {
		out[t] = &slotDemands{byTgt: map[int][]int{}, bySrc: map[int][]int{}}
	}
	for d := range inst.Demands {
		dem := &inst.Demands[d]
		for t := 0; t < T; t++ {
			if dem.Volume[t] <= 0 {
				continue
			}
			sd := out[t]
			sd.ids = append(sd.ids, d)
			if _, ok := sd.byTgt[dem.Target]; !ok {
				sd.tgtOrder = append(sd.tgtOrder, dem.Target)
			}
			if _, ok := sd.bySrc[dem.Source]; !ok {
				sd.srcOrder = append(sd.srcOrder, dem.Source)
			}
			sd.byTgt[dem.Target] = append(sd.byTgt[dem.Target], d)
			sd.bySrc[dem.Source] = append(sd.bySrc[dem.Source], d)
		}
	}
	return out
}

// blockedAt returns the blocked mask of slot t, tolerating a short/missing
// scenario slice.
func blockedAt(inst *model.Instance, t int) []bool {
	if t < 0 || t >= len(inst.Scenario.Blocked) {
		return nil
	}
	return inst.Scenario.Blocked[t]
}

// uniqueTightIncoming returns the single arc (x -> q) attaining
// dist(x, q) == w(x, q), or -1 when there are none or more than one.  dist must
// be the reverse Dijkstra from q.  Equal-length alternatives are deliberately
// *not* called dominated: ECMP splits between them, so neither is forced.
func uniqueTightIncoming(g *graph.Graph, blocked []bool, dist []float64, q int) int {
	if q < 0 || q >= g.N {
		return -1
	}
	found := -1
	for _, aid := range g.Ins[q] {
		if blocked != nil && blocked[aid] || g.From[aid] == q {
			continue
		}
		if dist[g.From[aid]] == g.Metric[aid] { // exact equality, as in ecmp
			if found >= 0 {
				return -1
			}
			found = aid
		}
	}
	return found
}

// uniqueTightOutgoing is the mirror of uniqueTightIncoming: the single arc
// (s -> y) attaining dist(s, y) == w(s, y).  dist must be the forward Dijkstra
// from s.
func uniqueTightOutgoing(g *graph.Graph, blocked []bool, dist []float64, s int) int {
	if s < 0 || s >= g.N {
		return -1
	}
	found := -1
	for _, aid := range g.Outs[s] {
		if blocked != nil && blocked[aid] || g.To[aid] == s {
			continue
		}
		if dist[g.To[aid]] == g.Metric[aid] { // exact equality, as in ecmp
			if found >= 0 {
				return -1
			}
			found = aid
		}
	}
	return found
}

// Judge holds the caches that a presolve judgement needs.  The per-(slot,
// source) distance arrays depend only on the instance (the base metric plus the
// maintenance blocks), not on the snapshot, so a Judge built once per solve
// serves both the initial scan and every later TestKey call.
type Judge struct {
	inst  *model.Instance
	g     *graph.Graph
	opts  Options
	slots []*slotDemands
	dist  [][]float64 // [t*N + s], lazily filled
	// scratch for the dodge digraph, rebuilt per (slot, arc)
	succ  [][]uint64 // one bitset row per tail: successors under R_e
	avoid []float64
	mask  []bool
	seen  []uint64 // BFS: nodes reached
	front []uint64 // BFS: current layer
	next  []uint64 // BFS: next layer
}

// NewJudge builds the per-solve judgement caches.
func NewJudge(inst *model.Instance, g *graph.Graph, opts Options) *Judge {
	wc := (g.N + 63) / 64
	j := &Judge{
		inst: inst, g: g, opts: opts,
		slots: indexDemands(inst),
		dist:  make([][]float64, inst.NSlots*g.N),
		succ:  make([][]uint64, g.N),
		mask:  make([]bool, g.M),
		avoid: make([]float64, g.N),
		seen:  make([]uint64, wc),
		front: make([]uint64, wc),
		next:  make([]uint64, wc),
	}
	for i := range j.succ {
		j.succ[i] = make([]uint64, wc)
	}
	return j
}

// Exact returns the volume (and demand count) of the demands of slot t that
// must cross arc e, using the class-3 dodge test.
//
// A route is a chain of shortest-path segments, so a demand can dodge e exactly
// when its target is *reachable from its source* in the digraph of
//
//	R_e(x, y)  <=>  some shortest x -> y path avoids e,
//
// because a chain s -> z -> ... -> t of such steps is itself a legal route (the
// intermediate nodes are the waypoints) that avoids e.  Demands whose target the
// source cannot reach that way load their whole volume on e.
//
// The relation is directed and must stay directed: R_e(x, y) does not imply
// R_e(y, x) (the shortest x -> y path avoiding e and the shortest y -> x path
// avoiding e are different objects).  Reaching for an undirected union-find
// closure instead silently claims dodges that no route realises -- it is
// *conservative* for the floor (it under-claims the forced set, and an earlier
// version honestly reported forced volume 0 for an arc the sprint reference
// proves is forced), but it hides exactly the cells this pass exists to find.
//
// Chains longer than the instance's segment budget are not filtered out: a
// too-long chain can only make the test *more* generous, i.e. it can only cause
// a floor to be missed, never a wrong floor to be claimed.
func (j *Judge) Exact(t, e int) (vol float64, ndem int) {
	g, inst := j.g, j.inst
	if t < 0 || t >= inst.NSlots || e < 0 || e >= g.M {
		return 0, 0
	}
	j.dodgeDigraph(t, e)
	sd := j.slots[t]
	cache := make(map[int][]uint64, 8) // per distinct source of this slot
	for _, d := range sd.ids {
		dem := &inst.Demands[d]
		if math.IsInf(j.distFor(t, dem.Source)[dem.Target], 1) {
			continue // unroutable at this slot: no sound conclusion
		}
		reach, ok := cache[dem.Source]
		if !ok {
			reach = j.reachFrom(dem.Source)
			cache[dem.Source] = reach
		}
		if !bitHas(reach, dem.Target) {
			vol += dem.Volume[t]
			ndem++
		}
	}
	return vol, ndem
}

// distFor returns the base-metric distances from source at slot t, cached.  The
// base metric keeps e present, so the cache is valid for every cell.
func (j *Judge) distFor(t, s int) []float64 {
	idx := t*j.g.N + s
	if d := j.dist[idx]; d != nil {
		return d
	}
	d := graph.Dijkstra(j.g, s, blockedAt(j.inst, t), false)
	j.dist[idx] = d
	return d
}

// dodgeDigraph rebuilds the successor rows of R_e for slot t.
//
// For every node x it compares the base distances out of x with the distances
// of the e-banned graph: a node y keeping its distance is an R_e successor of x.
// One shortcut keeps this affordable: when e lies on no shortest path out of x
// (its tail is not tight there), banning e changes nothing and x reaches every
// node it could reach before -- no second Dijkstra needed.  The rows are bitsets
// because reachFrom scans and unions them; at a few hundred nodes the whole
// digraph costs a few kilobytes.
func (j *Judge) dodgeDigraph(t, e int) {
	g := j.g
	blocked := blockedAt(j.inst, t)
	mask := j.mask
	for a := 0; a < g.M; a++ {
		mask[a] = (blocked != nil && blocked[a]) || a == e
	}
	tail, head := g.From[e], g.To[e]
	for x := 0; x < g.N; x++ {
		row := j.succ[x]
		for i := range row {
			row[i] = 0
		}
		base := j.distFor(t, x)
		// e lies on some shortest path out of x iff its tail is tight there.
		onSP := tail != head && !math.IsInf(base[tail], 1) && base[tail]+g.Metric[e] == base[head]
		if onSP {
			graph.DijkstraInto(g, x, mask, false, j.avoid)
		}
		for y := 0; y < g.N; y++ {
			if math.IsInf(base[y], 1) {
				continue // unreachable from x: no segment x -> y exists
			}
			if y == x || !onSP || j.avoid[y] == base[y] {
				bitSet(row, y)
			}
		}
	}
}

// reachFrom returns the set of nodes reachable from source in the current R_e
// digraph, including the source itself.  It is a plain BFS over the bitset rows:
// the frontier is a set of nodes, and one layer is the union of their successor
// rows minus what has been seen.
func (j *Judge) reachFrom(source int) []uint64 {
	copy(j.seen, j.succ[source])
	copy(j.front, j.succ[source])
	for !bitsEmpty(j.front) {
		for i := range j.next {
			j.next[i] = 0
		}
		bitsEach(j.front, func(y int) {
			row := j.succ[y]
			for i := range j.next {
				j.next[i] |= row[i]
			}
		})
		for i := range j.next {
			j.next[i] &^= j.seen[i]
			j.seen[i] |= j.next[i]
			j.front[i] = j.next[i]
		}
	}
	out := make([]uint64, len(j.seen))
	copy(out, j.seen)
	return out
}

func bitSet(b []uint64, i int)      { b[i>>6] |= 1 << uint(i&63) }
func bitHas(b []uint64, i int) bool { return b[i>>6]&(1<<uint(i&63)) != 0 }

func bitsEmpty(b []uint64) bool {
	for _, w := range b {
		if w != 0 {
			return false
		}
	}
	return true
}

// bitsEach calls f with the index of every set bit, lowest first.  Iterating
// while f writes its own (different) bitset is safe because the argument is
// j.front, which is never the target of the writes inside the loop body.
func bitsEach(b []uint64, f func(int)) {
	for wi, w := range b {
		for w != 0 {
			bit := w & -w
			f(wi<<6 + bits.TrailingZeros64(w))
			w &^= bit
		}
	}
}

// scanner couples a Judge with one run's report and Bounds.
type scanner struct {
	*Judge
	b           *Bounds
	sn          *snap.Snap
	rep         *Report
	start       time.Time
	limit       time.Duration
	pins        map[Key]Pin
	deeped      int
	structCells int
}

func (sc *scanner) expired() bool { return time.Since(sc.start) >= sc.limit }

// Run performs the one-shot scan of every slot against the snapshot and records
// the conclusions in b.  A timeout is not an error: the partial result is
// returned with TimedOut set.
func Run(inst *model.Instance, g *graph.Graph, sn *snap.Snap, b *Bounds, opts Options) (*Report, error) {
	if opts.Mode == Off {
		return &Report{}, nil
	}
	if b == nil {
		return nil, fmt.Errorf("presolve: nil Bounds")
	}
	sc := &scanner{
		Judge: NewJudge(inst, g, opts),
		b:     b, sn: sn, rep: &Report{},
		start: time.Now(), limit: opts.limit(),
		pins: map[Key]Pin{},
	}

	// Pass A (always complete): the cheap structural criteria over every slot.
	for t := 0; t < inst.NSlots; t++ {
		if sc.expired() {
			sc.rep.TimedOut = true
			break
		}
		sc.structural(t)
	}
	// Pass B (budgeted): the exact criterion on the hottest cells not yet
	// proven, in decreasing saturation order.
	if !sc.rep.TimedOut {
		sc.deep()
	}

	sc.rep.LBSat = b.SatFloor(g.Cap)
	sc.rep.Proven = len(b.ProvenKeys())
	sc.rep.Deep = sc.deeped
	sc.rep.Cells = sc.structCells
	for _, p := range sc.pins {
		sc.rep.Pins = append(sc.rep.Pins, p)
	}
	sort.Slice(sc.rep.Pins, func(i, j int) bool {
		if sc.rep.Pins[i].Sat != sc.rep.Pins[j].Sat {
			return sc.rep.Pins[i].Sat > sc.rep.Pins[j].Sat
		}
		if sc.rep.Pins[i].T != sc.rep.Pins[j].T {
			return sc.rep.Pins[i].T < sc.rep.Pins[j].T
		}
		return sc.rep.Pins[i].Arc < sc.rep.Pins[j].Arc
	})
	sc.rep.TimedOut = sc.rep.TimedOut || sc.expired()
	sc.rep.Elapsed = time.Since(sc.start)
	return sc.rep, nil
}

// structural runs classes 1 and 2 over one slot.
func (sc *scanner) structural(t int) {
	g, inst := sc.g, sc.inst
	blocked := blockedAt(inst, t)
	sd := sc.slots[t]
	T := inst.NSlots
	load := sc.sn.Load()

	for a := 0; a < g.M; a++ {
		if load[a*T+t] > 0 {
			sc.structCells++
		}
	}

	f := newForced()
	// Class 2a: sink dominance, one reverse distance array per target.
	for _, q := range sd.tgtOrder {
		dist := graph.Dijkstra(g, q, blocked, true)
		arc := uniqueTightIncoming(g, blocked, dist, q)
		if arc < 0 {
			continue
		}
		for _, d := range sd.byTgt[q] {
			if inst.Demands[d].Source == q {
				continue // degenerate s == t travels nowhere
			}
			f.add(arc, d, inst.Demands[d].Volume[t], "sink-dom")
		}
	}
	// Class 2b: source dominance, one forward distance array per source.
	for _, s := range sd.srcOrder {
		dist := graph.Dijkstra(g, s, blocked, false)
		arc := uniqueTightOutgoing(g, blocked, dist, s)
		if arc < 0 {
			continue
		}
		for _, d := range sd.bySrc[s] {
			if inst.Demands[d].Target == s {
				continue
			}
			f.add(arc, d, inst.Demands[d].Volume[t], "source-dom")
		}
	}
	// Class 1: the crossing direction of every cut edge.
	for _, br := range graph.Bridges(g, blocked) {
		side := graph.BridgeSide(g, blocked, br.U, br.V)
		for _, d := range sd.ids {
			dem := &inst.Demands[d]
			su, tu := side[dem.Source], side[dem.Target]
			if su == tu {
				continue
			}
			if su {
				f.add(br.ArcUV, d, dem.Volume[t], "bridge")
			} else {
				f.add(br.ArcVU, d, dem.Volume[t], "bridge")
			}
		}
	}
	sc.record(t, f)
}

// deep runs the exact criterion on the hottest cells of every slot, in
// decreasing saturation order, until the budget or MaxDeep stops it.
func (sc *scanner) deep() {
	g, inst := sc.g, sc.inst
	T := inst.NSlots
	load := sc.sn.Load()

	type cand struct {
		t, a int
		sat  float64
	}
	var cs []cand
	for t := 0; t < T; t++ {
		for a := 0; a < g.M; a++ {
			if load[a*T+t] <= 0 || g.Cap[a] <= 0 {
				continue
			}
			if bd, ok := sc.b.Get(t, a); ok && bd.Proven {
				continue
			}
			cs = append(cs, cand{t, a, load[a*T+t] / g.Cap[a]})
		}
	}
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].sat != cs[j].sat {
			return cs[i].sat > cs[j].sat
		}
		if cs[i].t != cs[j].t {
			return cs[i].t < cs[j].t
		}
		return cs[i].a < cs[j].a
	})
	for _, c := range cs {
		if sc.expired() || (sc.opts.MaxDeep > 0 && sc.deeped >= sc.opts.MaxDeep) {
			sc.rep.TimedOut = sc.rep.TimedOut || sc.expired()
			return
		}
		sc.deeped++
		f := newForced()
		if vol, ndem := sc.Exact(c.t, c.a); vol > 0 {
			f.setExact(c.a, vol, ndem, "dodge")
		}
		sc.record(c.t, f)
	}
}

// record turns one slot's forced sums into bounds.  The forced sum is always
// recorded as a floor; the cell is only *pinned* (lb == ub) when the observed
// load sits on that floor *and* no other demand of the slot can ever reach the
// arc.  A forced sum above the observed load is impossible for a sound
// argument, so the conclusion is revoked rather than trusted.
func (sc *scanner) record(t int, f *forced) {
	g, inst := sc.g, sc.inst
	T := inst.NSlots
	load := sc.sn.Load()
	tol := sc.opts.tol()
	for _, arc := range f.arcs() {
		if arc < 0 || arc >= g.M {
			continue
		}
		lb := f.vol[arc]
		if lb <= 0 {
			continue
		}
		cur := load[arc*T+t]
		thr := tol * math.Max(1, math.Abs(lb))
		if cur < lb-thr {
			// The forced sum exceeds the observed load, which is impossible for
			// a sound criterion: drop the conclusion rather than pin it.
			sc.rep.Revoked++
			if os.Getenv("TASR_PRESOLVE_VERBOSE") != "" {
				fmt.Printf("    presolve revoked: t=%d arc=%d(%d->%d) src=%s load=%.6f forced=%.6f\n",
					t, arc, inst.Arcs[arc].From, inst.Arcs[arc].To, f.tag(arc), cur, lb)
			}
			continue
		}
		// Sound for every routing: the floor (and the global first-bit floor
		// derived from it) never depends on the current snapshot.
		sc.b.PinLower(t, arc, lb)
		proven := math.Abs(cur-lb) <= thr && sc.noNewUser(t, arc, f)
		if proven {
			sc.b.PinProven(t, arc, cur, cur, f.tag(arc))
		}
		p := Pin{
			T: t, Arc: arc, Load: cur, LB: lb,
			Sat: cur / g.Cap[arc], Source: f.tag(arc),
			Proven: proven, Demands: f.ndem[arc],
		}
		if old, ok := sc.pins[Key{T: t, A: arc}]; !ok || (!old.Proven && p.Proven) {
			sc.pins[Key{T: t, A: arc}] = p
		}
	}
	sc.rep.Pinned = len(sc.pins)
}

// noNewUser reports whether every demand of slot t outside the forced set can
// never put load on arc e -- the missing half of an immovability proof.
//
// For a demand d some route of d reaches e = (u,v) with a detour through the
// arc whenever all three hold: s_d can reach u, v can reach t_d, and e lies on
// a shortest path (a route segment u -> v may then be inserted between the two
// shortest legs).  A demand therefore *cannot* touch e when at least one of
//
//	(a) u is unreachable from s_d,
//	(b) t_d is unreachable from v,
//	(c) e lies on no shortest path at all (w(e) > dist(u,v)),
//
// holds -- each is a property of the base graph and so is independent of the
// current routing.  A demand that fails all three keeps the cell unpinned: 宁漏
// 勿错, the conservative answer is "it might".
func (sc *scanner) noNewUser(t, e int, f *forced) bool {
	g, inst := sc.g, sc.inst
	u, v := g.From[e], g.To[e]
	if u == v {
		return false
	}
	du := sc.distFor(t, u) // dist(u, .)
	dv := sc.distFor(t, v) // dist(v, .)
	if g.Metric[e] > du[v] {
		return true // (c) e is on no shortest path, so on no route at all
	}
	for _, d := range sc.slots[t].ids {
		if f.dem[e][d] {
			continue // already forced onto e, counted exactly once
		}
		if math.IsInf(sc.distFor(t, inst.Demands[d].Source)[u], 1) {
			continue // (a)
		}
		if math.IsInf(dv[inst.Demands[d].Target], 1) {
			continue // (b)
		}
		return false
	}
	return true
}

// TestKey re-runs the exact judgement for one cell against the current snapshot
// and pins it when the forced volume covers the whole observed load *and* no
// other demand can reach the arc (see record).  This is the runtime hook the
// outer loop calls on a seed that keeps failing: a cell that turns out to be
// structural there stops consuming rounds.  It returns the new pin, or
// (nil, false) when the cell was not pinned (its floor is recorded either way).
func (j *Judge) TestKey(sn *snap.Snap, b *Bounds, t, arc int) (*Pin, bool) {
	if j.opts.Mode == Off || b == nil || arc < 0 || arc >= j.g.M || t < 0 || t >= j.inst.NSlots {
		return nil, false
	}
	if blocked := blockedAt(j.inst, t); blocked != nil && blocked[arc] {
		return nil, false // a down arc carries nothing to pin
	}
	if sn.Load()[arc*j.inst.NSlots+t] <= 0 {
		return nil, false
	}
	vol, ndem := j.Exact(t, arc)
	if vol <= 0 {
		return nil, false
	}
	sc := &scanner{
		Judge: j, b: b, sn: sn, rep: &Report{},
		start: time.Now(), limit: j.opts.limit(), pins: map[Key]Pin{},
	}
	before := len(b.ProvenKeys())
	f := newForced()
	f.setExact(arc, vol, ndem, "dodge")
	sc.record(t, f)
	if len(b.ProvenKeys()) == before {
		return nil, false
	}
	p := sc.pins[Key{T: t, A: arc}]
	return &p, true
}

// WritePins dumps a pin report as TSV for the experiment scripts.
func WritePins(path string, pins []Pin) error {
	fh, err := os.Create(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	w := bufio.NewWriter(fh)
	fmt.Fprintln(w, "slot\tarc\tload\tsat\tlb\tsource\tproven\tdemands")
	for _, p := range pins {
		g := func(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }
		fmt.Fprintf(w, "%d\t%d\t%s\t%s\t%s\t%s\t%t\t%d\n",
			p.T, p.Arc, g(p.Load), g(p.Sat), g(p.LB), p.Source, p.Proven, p.Demands)
	}
	return w.Flush()
}
