// Package local is the post-accept local search: a self-contained improvement
// pass the outer loop runs on a routing it has just accepted.
//
// It is deliberately not part of the MIP loop.  It consumes a Snap that is
// already legal -- the outer loop only calls it after an accepted round, so the
// incumbent has passed the lex gate and the Hamming budget -- and it never
// repairs an infeasible solution into a feasible one.  It depends on graph,
// snap, eval and model only; nothing here knows about Gurobi, candidate pools
// or the round schedule, which is what makes it testable on its own.
//
// One call runs at most Rounds rounds, each under its own wall-clock cap and
// each restarting the attack surface from the current saturation matrix:
//
//  1. Attack surface.  Every (slot, arc) cell is ranked by saturation, and the
//     hottest TopCells with saturation above SatMin are kept (default 20 above
//     0.01).  Only demands whose current path actually loads one of those cells
//     are touched at all.
//
//  2. Per demand.  The hottest cell the demand's own route carries is chosen,
//     and the segment of that route which carries the arc is located with the
//     same exact tight-arc test the candidate layer uses.  The arc is then
//     banned and the segment's shortest path recomputed: if the segment
//     disconnects, the arc is immovable and the demand is skipped -- that is
//     the only conclusion read off the banned model.
//
//  3. Detour waypoints.  The nodes of the post-ban path are candidate
//     waypoints; up to MaxProbes of them (default 5) are inserted into the
//     demand's current waypoint list, one per candidate, so the rest of the
//     demand's routing is preserved.  Degenerate insertions (the node is
//     already a waypoint, or equal to the segment's endpoints) and lists that
//     would exceed the scenario's max_segments are dropped.
//
//  4. Quality gate.  Each candidate is routed for real -- never under the ban,
//     which is the candidate layer's red line (0910cand.design §3.3): the ban
//     only discovers nodes, it never generates the load coefficients.  A
//     candidate is kept only when the resulting snapshot still respects the
//     Hamming budget and its truncated lexicographic vector is strictly better
//     than the current one; otherwise it is rolled back.
//
// A round in which no demand improves ends the call: the surface it was built
// on has not moved, so the next round would re-derive the same rejection.  The
// per-call cap is therefore a ceiling, not the expected cost -- an unproductive
// call pays for one round.
package local

import (
	"math"
	"sort"
	"time"

	"tasr/internal/eval"
	"tasr/internal/graph"
	"tasr/internal/model"
	"tasr/internal/snap"
)

// Defaults of Options.  DefaultCallBudget is the hard ceiling of one call, not
// of one round: the round loop stops at it wherever it is.
const (
	DefaultRounds     = 3
	DefaultCallBudget = 3 * time.Second
	DefaultSatMin     = 0.01
	DefaultTopCells   = 20
	DefaultMaxProbes  = 5
)

// Options configures a Searcher.  Zero values take the defaults above.
type Options struct {
	// Rounds bounds the round loop, CallBudget the whole call's wall clock.
	Rounds     int
	CallBudget time.Duration
	// SatMin and TopCells shape the attack surface: the hottest TopCells cells
	// whose saturation is strictly above SatMin.
	SatMin   float64
	TopCells int
	// MaxProbes bounds how many detour nodes are turned into candidates per
	// demand, and per round.
	MaxProbes int
	// Deadline is an external wall clock the call must not run past (the
	// solver's stopAt).  The earlier of it and CallBudget wins.
	Deadline time.Time
}

func (o Options) withDefaults() Options {
	if o.Rounds <= 0 {
		o.Rounds = DefaultRounds
	}
	if o.CallBudget <= 0 {
		o.CallBudget = DefaultCallBudget
	}
	if o.SatMin <= 0 {
		o.SatMin = DefaultSatMin
	}
	if o.TopCells <= 0 {
		o.TopCells = DefaultTopCells
	}
	if o.MaxProbes <= 0 {
		o.MaxProbes = DefaultMaxProbes
	}
	return o
}

// Result reports what one Run did.  Improved counts accepted moves (not
// distinct demands), Rounds the rounds actually started, and Elapsed the wall
// clock the call consumed -- the number the caller books against its own
// budget.  Skip breaks the non-improvements down by reason.
type Result struct {
	Rounds   int
	Improved int
	Demands  int
	Probes   int
	Bans     int
	Elapsed  time.Duration
	Skip     map[string]int
}

// Searcher runs the local search against one instance.  Build it once and reuse
// it across calls: it holds the graph index (and, when none is injected, the
// private one it falls back to), so hop and distance arrays survive between
// calls.  Not safe for concurrent use.
type Searcher struct {
	inst *model.Instance
	g    *graph.Graph
	ix   *graph.Index
	opts Options
}

// New builds a searcher.  ix may be nil, in which case a private index is built
// and kept for the searcher's lifetime; passing the solver's own index (the one
// the ECMP atom cache already uses) shares its memo instead of duplicating the
// Dijkstras.
func New(inst *model.Instance, g *graph.Graph, ix *graph.Index, opts Options) *Searcher {
	if ix == nil {
		ix = graph.NewIndex(g, inst.Scenario.Blocked, 0)
	}
	return &Searcher{inst: inst, g: g, ix: ix, opts: opts.withDefaults()}
}

// Run improves sn in place and reports what it did.  Every accepted move is a
// strict lexicographic improvement that respects the scenario budget, so the
// caller's incumbent invariants (and the emergency copy published from it) stay
// valid; the caller only has to re-read the saturation vector afterwards.
//
// sn is left untouched when nothing improves.
func (s *Searcher) Run(sn *snap.Snap) Result {
	st := Result{Skip: map[string]int{}}
	start := time.Now()
	deadline := start.Add(s.opts.CallBudget)
	if !s.opts.Deadline.IsZero() && s.opts.Deadline.Before(deadline) {
		deadline = s.opts.Deadline
	}
	// max_segments counts segments, so a path may hold at most MaxSegments-1
	// waypoints.  A scenario that allows none cannot be helped by this pass.
	maxWp := s.inst.Scenario.MaxSegments - 1
	if maxWp < 1 {
		st.Skip["no-waypoint-budget"]++
		st.Elapsed = time.Since(start)
		return st
	}
	for r := 0; r < s.opts.Rounds; r++ {
		if !time.Now().Before(deadline) {
			st.Skip["deadline"]++
			break
		}
		st.Rounds++
		if s.round(sn, maxWp, deadline, &st) == 0 {
			st.Skip["no-improvement"]++
			break
		}
	}
	st.Elapsed = time.Since(start)
	return st
}

// round runs one pass over the attack surface and returns how many moves it
// accepted.  desc is the running lex vector: it starts as the snapshot's own
// and is advanced by every accepted move, so each move is compared against the
// state the previous one produced rather than against the round's opener.
func (s *Searcher) round(sn *snap.Snap, maxWp int, deadline time.Time, st *Result) int {
	T := s.inst.NSlots
	sat := sn.Saturations()
	desc := eval.SortedDesc(sat)

	surface := s.surface(sn, sat, T)
	if len(surface) == 0 {
		st.Skip["no-surface"]++
		return 0
	}

	tasks := s.tasks(sn, surface, sat, T)
	improved := 0
	for i := range tasks {
		if !time.Now().Before(deadline) {
			st.Skip["deadline"]++
			break
		}
		st.Demands++
		if s.tryDemand(sn, tasks[i], maxWp, deadline, &desc, st) {
			improved++
		}
	}
	return improved
}

// surface returns the hottest TopCells cells whose saturation is above SatMin,
// hottest first.  RankKeys already breaks ties deterministically by (slot,
// arc), and the list is sorted descending, so the first cell at or below the
// floor ends the scan.
func (s *Searcher) surface(sn *snap.Snap, sat []float64, T int) []snap.Key {
	out := make([]snap.Key, 0, s.opts.TopCells)
	for _, k := range sn.RankKeys(nil) {
		if sat[k.A*T+k.T] <= s.opts.SatMin {
			break
		}
		out = append(out, k)
		if len(out) == s.opts.TopCells {
			break
		}
	}
	return out
}

// task is one (demand, cell) attempt: the demand d, the hot cell (t, a) it
// carries that is hottest over the whole surface, and that cell's saturation.
type task struct {
	d   int
	t   int
	a   int
	sat float64
}

// tasks picks, for every demand that loads part of the attack surface, the
// single hottest surface cell on its own route.  A demand with zero volume in
// the cell's slot is skipped outright -- the unit route still reports the arc,
// but the demand puts no load on it, so there is nothing to relieve.
//
// Slots and arcs are collected into sorted slices before the scan: ranging a
// map here would make which of two equally hot cells wins depend on Go's
// per-iteration map order.
func (s *Searcher) tasks(sn *snap.Snap, surface []snap.Key, sat []float64, T int) []task {
	slotSet := map[int]bool{}
	for _, k := range surface {
		slotSet[k.T] = true
	}
	slots := make([]int, 0, len(slotSet))
	for t := range slotSet {
		slots = append(slots, t)
	}
	sort.Ints(slots)

	out := make([]task, 0, len(s.inst.Demands))
	for d := range s.inst.Demands {
		dem := &s.inst.Demands[d]
		best := task{d: d, t: -1, a: -1, sat: -1}
		for _, t := range slots {
			if t < 0 || t >= T || dem.Volume[t] == 0 {
				continue
			}
			u, err := sn.UnitRoute(d, t, sn.GetWaypoints(d, t))
			if err != nil {
				continue // the current route is disconnected: not our business
			}
			for _, k := range surface {
				if k.T != t || u[k.A] <= 0 {
					continue
				}
				v := sat[k.A*T+k.T]
				if v > best.sat || (v == best.sat && (k.T < best.t || k.T == best.t && k.A < best.a)) {
					best = task{d: d, t: t, a: k.A, sat: v}
				}
			}
		}
		if best.sat >= 0 {
			out = append(out, best)
		}
	}
	// Hottest cell first, demand index as the deterministic tie-break.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].sat != out[j].sat {
			return out[i].sat > out[j].sat
		}
		if out[i].t != out[j].t {
			return out[i].t < out[j].t
		}
		if out[i].a != out[j].a {
			return out[i].a < out[j].a
		}
		return out[i].d < out[j].d
	})
	return out
}

// tryDemand discovers the detour candidates for one (demand, cell) and applies
// the first one that passes the quality gate.  At most one move is accepted per
// demand per round: once the demand's route changes, the detour it was derived
// from describes a path that no longer exists.
func (s *Searcher) tryDemand(sn *snap.Snap, tk task, maxWp int, deadline time.Time, desc *[]float64, st *Result) bool {
	g := s.g
	dem := &s.inst.Demands[tk.d]
	t, e := tk.t, tk.a
	wps := sn.GetWaypoints(tk.d, t)
	pts := make([]int, 0, len(wps)+2)
	pts = append(pts, dem.Source)
	pts = append(pts, wps...)
	pts = append(pts, dem.Target)

	// Which segment of the current route carries e.  The tight-arc test is the
	// candidate layer's exact-equality one (CLAUDE.md red line): both sides come
	// from the same Dijkstra, so bit equality is reliable and a tolerance would
	// drift away from the arcs the deployed routing actually uses.
	seg := -1
	for i := 0; i+1 < len(pts); i++ {
		if pts[i] == pts[i+1] {
			continue
		}
		if s.carries(t, pts[i], pts[i+1], e) {
			seg = i
			break
		}
	}
	if seg < 0 {
		st.Skip["off-segment"]++
		return false
	}
	p, q := pts[seg], pts[seg+1]

	// Ban the arc and re-route the segment.  A disconnected segment is the one
	// usable conclusion from the banned model: the arc cannot be moved.
	blocked := withBan(s.inst, g, t, e)
	dist := graph.Dijkstra(g, p, blocked, false)
	if math.IsInf(dist[q], 1) {
		st.Skip["unmovable"]++
		return false
	}
	st.Bans++
	path := pathTo(g, dist, p, q)
	if len(path) < 3 {
		st.Skip["no-detour"]++
		return false
	}

	mids := make([]int, 0, len(path))
	seen := map[int]bool{}
	for _, w := range path[1 : len(path)-1] {
		if w == dem.Source || w == dem.Target || w == p || w == q || inList(wps, w) || seen[w] {
			continue
		}
		seen[w] = true
		mids = append(mids, w)
	}
	if len(mids) == 0 {
		st.Skip["no-mid"]++
		return false
	}

	for _, u := range spread(mids, s.opts.MaxProbes) {
		if !time.Now().Before(deadline) {
			st.Skip["deadline"]++
			break
		}
		st.Probes++
		cand := insertAt(wps, seg, u)
		if len(cand) > maxWp {
			st.Skip["max-wp"]++
			continue
		}
		if equalInts(cand, wps) {
			st.Skip["same-path"]++
			continue
		}
		old, err := sn.SetWaypoints(tk.d, t, cand)
		if err != nil {
			// The candidate route is not deployable (a segment is down at this
			// slot).  SetWaypoints has already rolled its own subtraction back.
			st.Skip["route"]++
			continue
		}
		if !sn.BudgetOK() {
			sn.SetWaypoints(tk.d, t, old)
			st.Skip["budget"]++
			continue
		}
		next := eval.SortedDesc(sn.Saturations())
		if eval.LexCompare(next, *desc) < 0 {
			*desc = next
			st.Improved++
			st.Skip["accepted"]++
			return true
		}
		sn.SetWaypoints(tk.d, t, old)
		st.Skip["lex"]++
	}
	return false
}

// carries reports whether arc e lies on a shortest p -> q path, i.e. p ->
// From[e] -> To[e] -> q is as short as p -> q.
func (s *Searcher) carries(t, p, q, e int) bool {
	g := s.g
	df := s.ix.DistFrom(t, p)
	base := df[q]
	if math.IsInf(base, 1) {
		return false
	}
	dt := s.ix.DistTo(t, q)
	return df[g.From[e]]+g.Metric[e]+dt[g.To[e]] == base
}

// insertAt inserts waypoint u between pts[seg] and pts[seg+1] of the expanded
// path [source, wps..., target], which in waypoint coordinates is wps[:seg] +
// u + wps[seg:].
func insertAt(wps []int, seg, u int) []int {
	if seg < 0 {
		seg = 0
	}
	if seg > len(wps) {
		seg = len(wps)
	}
	out := make([]int, 0, len(wps)+1)
	out = append(out, wps[:seg]...)
	out = append(out, u)
	out = append(out, wps[seg:]...)
	return out
}

// spread picks at most n entries of xs, evenly spaced and in order, so the
// probes are distributed along the detour instead of all clustering at its
// start.
func spread(xs []int, n int) []int {
	if n <= 0 || len(xs) <= n {
		return xs
	}
	out := make([]int, 0, n)
	last := -1
	for i := 0; i < n; i++ {
		j := i * (len(xs) - 1) / (n - 1)
		if j == last {
			continue
		}
		last = j
		out = append(out, xs[j])
	}
	return out
}

func inList(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// withBan returns slot t's blocked mask plus arc e, without mutating the
// scenario's own slice.
func withBan(inst *model.Instance, g *graph.Graph, t, e int) []bool {
	out := make([]bool, g.M)
	if t >= 0 && t < len(inst.Scenario.Blocked) {
		copy(out, inst.Scenario.Blocked[t])
	}
	out[e] = true
	return out
}

// pathTo reconstructs the node sequence of a shortest p -> q path from a
// forward distance array.  Predecessors are taken in ascending arc id (the
// graph's adjacency order), so the walk is deterministic.
func pathTo(g *graph.Graph, dist []float64, p, q int) []int {
	rev := make([]int, 0, g.N)
	node := q
	for node != p {
		if len(rev) > g.N {
			return nil // no predecessor chain: dist and topology disagree
		}
		best := -1
		for _, a := range g.Ins[node] {
			from := g.From[a]
			if dist[from]+g.Metric[a] == dist[node] {
				best = a
				break
			}
		}
		if best < 0 {
			return nil
		}
		rev = append(rev, node)
		node = g.From[best]
	}
	rev = append(rev, p)
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev
}
