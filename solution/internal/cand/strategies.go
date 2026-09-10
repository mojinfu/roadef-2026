package cand

import (
	"math"
	"sort"

	"tasr/internal/graph"
	"tasr/internal/model"
)

// stratPool is one strategy's contribution: an ordered node list (the raw
// material for 1-waypoint candidates, in the strategy's own priority order) and
// the explicit node pairs it wants to see combined into 2-waypoint candidates.
//
// pairs is residual's alone.  hot_center and od_scan used to fill it too -- every
// pair of their pooled nodes, or of the offload-proven subset -- and bottleneck
// crossed two different hot arcs' detours.  A pair assembled from two
// independently chosen singletons composes by accident: nothing in the search
// makes the second turning point complementary to the first.  That is the shape
// residual was written to replace, so the 2-waypoint side is now residual's
// alone and these strategies leave the field nil.
type stratPool struct {
	nodes []Node
	pairs [][2]int
	// pairTag labels this pool's pairs in the tag histogram.  Each strategy
	// names its own so a composed mode ("mix+residual") still attributes a pair
	// to the strategy that proposed it rather than to the composition.
	pairTag string
}

// hopBall is the primitive all three strategies start from: the nodes whose
// detour s->w->tgt costs at most maxExtra hops more than the direct shortest
// path, capped at cap nodes and ordered by that extra cost.
//
//	hf[w] = hops s->w, ht[w] = hops w->tgt (both directed, slot t's arcs applied)
//	Hmin  = hf[tgt] = the current minimum hop count
//	ball  = { w : hf[w] >= 0 && ht[w] >= 0 && hf[w]+ht[w] <= Hmin+maxExtra }
//
// Growing an extra-radius one step at a time and stopping at `cap` is
// equivalent to keeping the `cap` nodes with the smallest hf+ht, which is what
// this does directly -- it never returns fewer nodes than the incremental form.
// A node unreachable from either side is skipped: it cannot sit on any s->tgt
// path, so no waypoint list through it is usable.
func hopBall(hp Hops, dem *model.Demand, t, cap, maxExtra int) []Node {
	hf := hp.ForwardAt(t, dem.Source)
	ht := hp.ReverseAt(t, dem.Target)
	if hf[dem.Target] < 0 {
		return nil // source and target already disconnected at this slot
	}
	limit := hf[dem.Target] + maxExtra
	out := make([]Node, 0, cap)
	for w := 0; w < len(hf); w++ {
		if w == dem.Source || w == dem.Target {
			continue
		}
		if hf[w] < 0 || ht[w] < 0 {
			continue
		}
		h := hf[w] + ht[w]
		if h > limit {
			continue
		}
		out = append(out, Node{ID: w, Hops: h})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Hops != out[j].Hops {
			return out[i].Hops < out[j].Hops
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > cap {
		out = out[:cap]
	}
	return out
}

// hotCenter is strategy 1: rank the hop ball by undirected hop distance to the
// two endpoints of the round's hot arc, so the capped pool is spent on nodes
// that can actually move load off that arc.
//
// The ranking is a *preference*, not a judgement: because it deliberately
// ignores flow direction, a node that sits geometrically close to the hot arc
// but downstream of it can be ranked in while being useless.  That is
// affordable because the pool is capped at PoolCap instead of n-2 and because
// Build routes every emitted candidate for real anyway.
//
// It used to spend one extra routing per pooled node proving which nodes could
// serve as pairing material -- at the time the sole difference from od_scan.
// With the 2-waypoint side now residual's, that filter has nothing left to
// gate: the pool is returned whole, exactly as it was before (the filter never
// removed a node from it), so dropping the loop changes no candidate and saves
// the routings of nodes the interleave cap drops anyway.
func hotCenter(hp Hops, g *graph.Graph, inst *model.Instance,
	fam Family, opts Options) stratPool {
	dem := &inst.Demands[fam.D]
	var perSlot [][]Node
	for _, t := range fam.Slots {
		ball := hopBall(hp, dem, t, opts.PoolCap, opts.MaxExtraHop)
		if len(ball) == 0 {
			continue
		}
		rankByHotDistance(hp, g, t, fam.Hots[t], ball)
		for i := range ball {
			ball[i].Tag = ModeHotCenter
		}
		perSlot = append(perSlot, ball)
	}
	return stratPool{nodes: interleave(perSlot, opts.PoolCap)}
}

// rankByHotDistance sorts the ball by min undirected hop distance to any
// endpoint of a hot arc; nodes with no reachable center keep +inf distance and
// fall to the back while retaining hop order among themselves.
func rankByHotDistance(hp Hops, g *graph.Graph, t int, arcs []int, ball []Node) {
	if len(arcs) == 0 {
		return // nothing hot at this slot: the ball's hop order already is the ranking
	}
	centers := map[int]bool{}
	for _, e := range arcs {
		centers[g.From[e]] = true
		centers[g.To[e]] = true
	}
	dist := make(map[int]int, len(ball))
	for c := range centers {
		h := hp.UndirectedAt(t, c)
		for _, n := range ball {
			if h[n.ID] < 0 {
				continue
			}
			if prev, ok := dist[n.ID]; !ok || h[n.ID] < prev {
				dist[n.ID] = h[n.ID]
			}
		}
	}
	for i := range ball {
		if d, ok := dist[ball[i].ID]; ok {
			ball[i].Dist = d
		} else {
			ball[i].Dist = math.MaxInt32
		}
	}
	sort.SliceStable(ball, func(i, j int) bool { return ball[i].Dist < ball[j].Dist })
}

// odScan is strategy 2: the same hop ball, ordered by pure extra hops (which is
// the order hopBall already produces) with no hot-arc knowledge whatsoever.
//
// Its reason to exist is the cap.  hot_center fills the merge with nodes that
// are near the hot arc, so the long detours -- the ones that move load off the
// tail of the vector rather than off the max -- would never make it into a
// 24-node pool.  od_scan keeps them.  It is also strictly cheaper: no extra
// BFS per hot-arc endpoint.
//
// It used to hand every pooled pair to Build as 2-waypoint material, which is
// C(pool,2) routings per family for a pair no hotter than the two singletons it
// was made of.  That is the enumeration the mix retired.
func odScan(hp Hops, inst *model.Instance, fam Family, opts Options) stratPool {
	dem := &inst.Demands[fam.D]
	var perSlot [][]Node
	for _, t := range fam.Slots {
		ball := hopBall(hp, dem, t, opts.PoolCap, opts.MaxExtraHop)
		for i := range ball {
			ball[i].Tag = ModeODScan
		}
		perSlot = append(perSlot, ball)
	}
	return stratPool{nodes: interleave(perSlot, opts.PoolCap)}
}

// globalRelief is the safety net: route *every* node as a singleton and keep
// the GlobalK that relieve a target the most.
//
// The scan is deliberately not hop-bounded.  Bounding it by the hop ball is
// exactly what lost the node on setA-04/10/13/16: the best singleton by relief
// can sit outside a ball grown by MaxExtraHop=4, and no amount of ranking inside
// that ball can recover a node that was never admitted.
//
// Cost is O(n) routings per family -- the same w1 cost the legacy brute-force
// generator paid -- and it shares routeMemo with the strategies, so a node they
// already routed is a map lookup here rather than a second routing.  Only the
// singletons are scanned: the 2-waypoint side has no brute-force counterpart and
// is not what the recall measurement found missing.
func globalRelief(memo *routeMemo, inst *model.Instance, fam Family, opts Options) []Node {
	dem := &inst.Demands[fam.D]
	type scored struct {
		id  int
		rel float64
	}
	var kept []scored
	for w := 0; w < inst.NNodes(); w++ {
		if w == dem.Source || w == dem.Target {
			continue
		}
		// Same acceptance test the emitted candidates get: every family slot must
		// route, and the best relief across slots must be strictly positive.
		best := -1.0
		usable := true
		for _, t := range fam.Slots {
			u, ok := memo.at(t, []int{w})
			if !ok {
				usable = false
				break
			}
			if r := fam.Relief(t, u); r > best {
				best = r
			}
		}
		if !usable || best <= FracEps {
			continue
		}
		kept = append(kept, scored{id: w, rel: best})
	}
	// Stable sort on ascending id order, so equal relief breaks toward the lower
	// node id and the selection is reproducible from the instance alone.
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].rel > kept[j].rel })
	if len(kept) > opts.GlobalK {
		kept = kept[:opts.GlobalK]
	}
	out := make([]Node, 0, len(kept))
	for _, s := range kept {
		out = append(out, Node{ID: s.id, Tag: TagGlobal})
	}
	return out
}

// bottleneck is strategy 3: find the round's hot arcs that the demand actually
// crosses, ban each in turn, and take the intermediate nodes of the resulting
// detour.  Crossing the detour node sets of two different hot arcs yields
// 2-waypoint candidates that relieve both at once.
//
// That cross product is the archetype of the 2-waypoint generation the mix
// retired -- two detours computed independently on the original path, glued
// together in the hope that they compose -- and it is no longer part of
// ModeMix, where residual takes the pair slot.  The strategy is left intact and
// selectable on its own ("-cand-mode bottleneck") so the two shapes stay
// A/B-able; its detour-node pool is the part the mix no longer draws on.
//
// RED LINE: the ban is only how the detour nodes are *discovered*.  The detour
// path itself is never stored as a candidate -- the deployed semantics ban
// nothing beyond the scenario's own blocked arcs, so a stored detour path would
// be re-routed straight back over the hot arc and the MIP's load coefficient
// would no longer describe the real routing (it would think the arc was
// unloaded when it was not).  Only the nodes travel; the caller re-routes every
// candidate from scratch through UnitRoute.
func bottleneck(memo *routeMemo, ix *graph.Index, g *graph.Graph,
	inst *model.Instance, fam Family, opts Options) stratPool {
	dem := &inst.Demands[fam.D]
	var detours [][]int
	banned := 0

	for _, t := range fam.Slots {
		arcs := fam.Hots[t]
		if len(arcs) == 0 || banned >= opts.MaxBans {
			continue
		}
		hot := crossedHotArcs(ix, g, dem, t, memo.wps(t), arcs)
		for _, e := range hot {
			if banned >= opts.MaxBans {
				// Rate limit: a demand whose segment path crosses many hot arcs
				// would otherwise pay one Dijkstra each.  A handful of bans is
				// enough to surface the detour structure.
				break
			}
			banned++
			blocked := withBan(inst, g, t, e)
			dist := graph.Dijkstra(g, dem.Source, blocked, false)
			if math.IsInf(dist[dem.Target], 1) {
				continue // no detour exists at all with this arc removed
			}
			var nodes []int
			for _, w := range pathTo(g, dist, dem.Source, dem.Target) {
				if w == dem.Source || w == dem.Target {
					continue
				}
				nodes = append(nodes, w)
			}
			if len(nodes) > 0 {
				detours = append(detours, nodes)
			}
		}
	}

	// Pool: every detour node, in discovery order.  Pairs: one node from the
	// detour of one hot arc crossed with one from the detour of another, so the
	// resulting waypoint list avoids both.
	var nodes []Node
	seen := map[int]bool{}
	for _, d := range detours {
		for _, w := range d {
			if !seen[w] {
				seen[w] = true
				nodes = append(nodes, Node{ID: w, Tag: ModeBottleneck})
			}
		}
	}
	var pairs [][2]int
	for i := 0; i < len(detours); i++ {
		for j := i + 1; j < len(detours); j++ {
			for _, a := range detours[i] {
				for _, b := range detours[j] {
					pairs = append(pairs, [2]int{a, b})
				}
			}
		}
	}
	return stratPool{nodes: nodes, pairs: pairs}
}

// crossedHotArcs returns the subset of arcs that lie on a shortest s->tgt path
// through the demand's current segments.  An arc a is tight for segment p->q
// iff routing p -> From[a] -> To[a] -> q is as short as p -> q:
//
//	DistFrom(t,p)[From[a]] + Metric[a] + DistTo(t,q)[To[a]] == DistFrom(t,p)[q]
//
// The comparison is exact, matching the ECMP code's floating-point convention
// (CLAUDE.md red line): both sides come from the same Dijkstra, relaxed in the
// same fixed order, so bit-exact equality is reliable.  Loosening it to a
// tolerance would drift the tight-arc set away from the one the deployed
// routing actually uses.
func crossedHotArcs(ix *graph.Index, g *graph.Graph, dem *model.Demand,
	t int, wps []int, arcs []int) []int {
	if len(arcs) == 0 {
		return nil
	}
	var out []int
	seen := map[int]bool{}
	for _, seg := range segmentsOf(dem, wps) {
		p, q := seg[0], seg[1]
		df := ix.DistFrom(t, p)
		dt := ix.DistTo(t, q)
		base := df[q]
		if math.IsInf(base, 1) {
			continue
		}
		// Iterate arcs, not a set built from it: Go randomises map iteration
		// order per iteration, and the caller rate-limits itself to MaxBans
		// bans in the order returned here.  Ranging a map would make *which*
		// hot arcs get banned -- and therefore the whole candidate set --
		// vary run to run, which is the nondeterminism the benchmark measured.
		for _, e := range arcs {
			if seen[e] {
				continue
			}
			// The arc must leave p and enter q through the segments' own nodes.
			// Checking the tight inequality at both ends is enough: it makes
			// p->From[e]->To[e]->q a shortest walk, hence e lies on a shortest
			// p->q path.
			if df[g.From[e]]+g.Metric[e]+dt[g.To[e]] == base {
				out = append(out, e)
				seen[e] = true
			}
		}
	}
	return out
}

// segmentsOf expands a waypoint list into the (from, to) pairs the router
// actually walks: source, then the waypoints, then target, dropping empty
// hops.  Mirrors snap.Snap.segments.
func segmentsOf(dem *model.Demand, wps []int) [][2]int {
	pts := make([]int, 0, len(wps)+2)
	pts = append(pts, dem.Source)
	pts = append(pts, wps...)
	pts = append(pts, dem.Target)
	var segs [][2]int
	for i := 0; i+1 < len(pts); i++ {
		if pts[i] != pts[i+1] {
			segs = append(segs, [2]int{pts[i], pts[i+1]})
		}
	}
	return segs
}

// withBans returns slot t's blocked mask plus the given arcs, without mutating
// the scenario's own slice.  Every strategy that bans arcs to *discover* nodes
// goes through here, so the ban is always layered on the deployed blocked set
// rather than replacing it.
func withBans(inst *model.Instance, g *graph.Graph, t int, arcs ...int) []bool {
	out := make([]bool, g.M)
	if t >= 0 && t < len(inst.Scenario.Blocked) {
		copy(out, inst.Scenario.Blocked[t])
	}
	for _, e := range arcs {
		out[e] = true
	}
	return out
}

// withBan is the single-arc form of withBans.
func withBan(inst *model.Instance, g *graph.Graph, t, e int) []bool {
	return withBans(inst, g, t, e)
}

// residual is strategy 4, 残差串行 (residual serial): build a 2-waypoint
// candidate whose two turning points are *complementary* -- the second is
// discovered on the path the first one already produced -- instead of crossing
// two single-arc detours that were each computed on the original path, which is
// what bottleneck does.
//
// The premise is a demand whose current path presses at least ResidualMinHot of
// the round's hot arcs.  That is the case a pair is for: relieving only the
// first of them just walks onto the next one, and two independent detours need
// not compose into a route that avoids either.
//
// Per family slot:
//
//  1. A is the hottest of the crossed arcs (fam.Hots is hottest-first).  Ban A
//     and take U from the *shortest-path DAG that survives the ban*
//     (df[w]+dt[w] == df[tgt]) -- the nodes any route around A must pass
//     through.  Ranked by undirected hop distance to A's endpoints and capped
//     at ResidualU.  The ranking is hot_center's, but the DAG replaces its hop
//     ball: the ball is grown against the *unbanned* shortest path, which is
//     exactly the arc we are trying to leave.
//  2. Walk the A-free S->U->T path ("the new path").  If it presses no other
//     hot arc, one point is enough -- U alone is the candidate and no pair is
//     built, because a second turning point could only add Hamming cost.
//  3. Otherwise B is the hottest hot arc it still presses.  Ban B *on top of* A
//     and take V from the {A,B}-free residual S->U->T: only the nodes the B-ban
//     actually introduced, since a node already on the A-free path was
//     reachable without banning B and is no turning point.  Both orders (U,V)
//     and (V,U) are tried -- which one routes better is the deployed router's
//     call, not ours.
//  4. Every surviving candidate must, re-routed for real, unload A
//     (Family.ArcRelief).  The pair exists to unload A *and* B together; a
//     candidate that relieves only something else is not this strategy's
//     material, and the MIP's load-signature dedup already drops a pair that
//     routes exactly like one of its own singletons.
//
// RED LINE, same as bottleneck: the bans only *discover* nodes.  No candidate
// carries its ban context -- the emitted waypoint list is re-routed from
// scratch (here through routeMemo, which is the same routing Build step 5 would
// have paid for anyway).  The exact `==` in the DAG test is the codebase's
// usual tightness convention (CLAUDE.md red line).  Unlike the ECMP path it
// cannot corrupt a coefficient -- a node wrongly admitted or dropped only
// changes what is *proposed* -- so the convention is kept for determinism.
func residual(memo *routeMemo, ix *graph.Index, hp Hops, g *graph.Graph,
	inst *model.Instance, fam Family, opts Options) stratPool {

	dem := &inst.Demands[fam.D]

	// cand is one validated candidate: v < 0 is a bare singleton (step 2's "one
	// point is enough"), otherwise the ordered waypoint list (u, v).  They are
	// collected first and ordered once at the end so the emitted pool is ranked
	// by relief rather than merely in slot order.
	type cand struct {
		u, v int
		rel  float64
	}
	var cands []cand

	// gate is the step-4 acceptance test.  It reports the candidate's relief on
	// arc A specifically, which is also its ranking key.  Without ArcRelief the
	// caller has given us no way to isolate one cell, so the family-wide relief
	// stands in -- weaker (it admits a candidate that relieves a different hot
	// arc) but never empty.
	gate := func(t, arc int, unit []float64) (float64, bool) {
		if fam.ArcRelief != nil {
			r := fam.ArcRelief(t, arc, unit)
			return r, r > FracEps
		}
		r := fam.Relief(t, unit)
		return r, r > FracEps
	}

	for _, t := range fam.Slots {
		hot := fam.Hots[t]
		if len(hot) == 0 {
			continue
		}
		// crossedHotArcs keeps the order of `hot`, so crossed[0] is the hottest
		// arc the demand actually presses -- step 1's A.
		crossed := crossedHotArcs(ix, g, dem, t, memo.wps(t), hot)
		if len(crossed) < opts.ResidualMinHot {
			continue // the premise: one hot arc is a singleton's job, not a pair's
		}
		a := crossed[0]

		blockedA := withBans(inst, g, t, a)
		dfA := graph.Dijkstra(g, dem.Source, blockedA, false)
		dtA := graph.Dijkstra(g, dem.Target, blockedA, true)
		if math.IsInf(dfA[dem.Target], 1) {
			continue // A is a bridge here: no route avoids it, nothing to discover
		}
		us := shortestDag(dfA, dtA, dem.Source, dem.Target)
		if len(us) == 0 {
			continue
		}
		rankByHotDistance(hp, g, t, []int{a}, us)
		if len(us) > opts.ResidualU {
			us = us[:opts.ResidualU]
		}

		// Steps 2-3: which hot arc each U's A-free path still presses.  The U's
		// are grouped by B so the {A,B} Dijkstras are paid once per *distinct B*
		// rather than once per U -- the bans are what cost, not the walks.
		type job struct {
			u    int
			path []int // the A-free S->U->T node walk
		}
		type group struct {
			b    int
			jobs []job
		}
		var bare []int
		var groups []group
		byB := map[int]int{}
		for _, un := range us {
			p := pathTo(g, dfA, dem.Source, un.ID)
			q := pathFrom(g, dtA, un.ID, dem.Target)
			if p == nil || q == nil {
				continue
			}
			path := joinPaths(p, q)
			b := hottestArcOnPath(g, path, hot, a)
			if b < 0 {
				bare = append(bare, un.ID)
				continue
			}
			if gi, ok := byB[b]; ok {
				groups[gi].jobs = append(groups[gi].jobs, job{u: un.ID, path: path})
				continue
			}
			byB[b] = len(groups)
			groups = append(groups, group{b: b, jobs: []job{{u: un.ID, path: path}}})
		}

		// Step 2: no second hot arc on the new path.  U alone is the candidate.
		for _, u := range bare {
			unit, ok := memo.at(t, []int{u})
			if !ok {
				continue
			}
			if rel, pass := gate(t, a, unit); pass {
				cands = append(cands, cand{u: u, v: -1, rel: rel})
			}
		}

		// Step 3: the serial pair.  V comes from the residual path that already
		// goes through U and avoids A, which is what makes the two points
		// complementary rather than two detours glued together.
		for _, gr := range groups {
			blockedAB := withBans(inst, g, t, a, gr.b)
			dfAB := graph.Dijkstra(g, dem.Source, blockedAB, false)
			dtAB := graph.Dijkstra(g, dem.Target, blockedAB, true)
			for _, jb := range gr.jobs {
				if math.IsInf(dfAB[jb.u], 1) || math.IsInf(dtAB[jb.u], 1) {
					continue // banning B cuts U off: no residual path runs through it
				}
				p := pathTo(g, dfAB, dem.Source, jb.u)
				q := pathFrom(g, dtAB, jb.u, dem.Target)
				if p == nil || q == nil {
					continue
				}
				onA := make(map[int]bool, len(jb.path))
				for _, w := range jb.path {
					onA[w] = true
				}
				vs := residualVs(joinPaths(p, q), onA, dem.Source, dem.Target, jb.u, opts.ResidualV)
				for _, v := range vs {
					for _, wps := range [2][]int{{jb.u, v}, {v, jb.u}} {
						unit, ok := memo.at(t, wps)
						if !ok {
							continue
						}
						rel, pass := gate(t, a, unit)
						if !pass {
							continue
						}
						cands = append(cands, cand{u: wps[0], v: wps[1], rel: rel})
					}
				}
			}
		}
	}

	// Ordering is where this strategy pays for the pool merge, so it is not the
	// plain relief order the other strategies use:
	//
	//   - pairs before bare singletons.  Build's merge is round-robin across
	//     strategies and stops at PoolCap, so with four strategies running a
	//     strategy is worth about PoolCap/4 nodes -- roughly three pairs.  A
	//     bare U is a singleton every other strategy can find; the serial pair
	//     is what only this strategy produces, so it goes first.
	//   - within each group, relief descending (stable: ties keep discovery
	//     order, so the result is reproducible).
	//   - a pair's endpoints are written adjacently, addNode(u) first.  Step 3
	//     of Build keeps only pairs whose endpoints both survived the merge, so
	//     a V stranded behind the other U's would silently drop the pair it
	//     belongs to.
	sort.SliceStable(cands, func(i, j int) bool {
		if (cands[i].v < 0) != (cands[j].v < 0) {
			return cands[i].v >= 0
		}
		return cands[i].rel > cands[j].rel
	})
	var nodes []Node
	var pairs [][2]int
	seenNode := map[int]bool{}
	seenPair := map[[2]int]bool{}
	for _, c := range cands {
		if !seenNode[c.u] {
			seenNode[c.u] = true
			nodes = append(nodes, Node{ID: c.u, Tag: ModeResidual})
		}
		if c.v < 0 {
			continue
		}
		if !seenNode[c.v] {
			seenNode[c.v] = true
			nodes = append(nodes, Node{ID: c.v, Tag: ModeResidual})
		}
		if pr := [2]int{c.u, c.v}; !seenPair[pr] {
			seenPair[pr] = true
			pairs = append(pairs, pr)
		}
	}
	return stratPool{nodes: nodes, pairs: pairs, pairTag: ModeResidual}
}

// shortestDag returns the nodes that lie on some shortest source->target path
// under the blocked set the two distance arrays were computed with -- the
// shortest-path DAG.  These are the nodes any route from source to target still
// passes through, which is where a turning point has to sit to be useful.
//
// The `==` is exact, the codebase's usual tightness convention (design doc
// §3.0.2).  A node wrongly admitted or dropped only changes what the strategy
// proposes -- every candidate is re-routed for real before it can be used -- so
// this is a determinism choice, not a checker-alignment one.
func shortestDag(df, dt []float64, source, target int) []Node {
	total := df[target]
	if math.IsInf(total, 1) {
		return nil
	}
	var out []Node
	for w := 0; w < len(df); w++ {
		if w == source || w == target {
			continue
		}
		if math.IsInf(df[w], 1) || math.IsInf(dt[w], 1) {
			continue
		}
		if df[w]+dt[w] == total {
			out = append(out, Node{ID: w})
		}
	}
	return out
}

// hottestArcOnPath returns the hottest arc of `hot` that the node walk crosses,
// ignoring `exclude`; -1 when it crosses none of them.  `hot` is the round's
// hot list, hottest first, so taking the first hit is what makes this "the
// hottest arc the new path still presses" and not merely "an arc it presses".
func hottestArcOnPath(g *graph.Graph, path []int, hot []int, exclude int) int {
	if len(path) < 2 {
		return -1
	}
	on := map[int]bool{}
	for i := 0; i+1 < len(path); i++ {
		if e := arcOn(g, path[i], path[i+1]); e >= 0 {
			on[e] = true
		}
	}
	for _, e := range hot {
		if e != exclude && on[e] {
			return e
		}
	}
	return -1
}

// arcOn is the arc the walk took from one node to the next: the first entry of
// the adjacency list with that head.  Adjacency is in ascending arc-id order,
// so the choice is deterministic; -1 means the walk is not a walk.
func arcOn(g *graph.Graph, from, to int) int {
	for _, a := range g.Outs[from] {
		if g.To[a] == to {
			return a
		}
	}
	return -1
}

// residualVs picks step 3's V turning points: the intermediate nodes of the
// {A,B}-free residual S->U->T that banning B actually introduced.  A node that
// also lies on the A-free path (onA) was reachable without banning B, so it is
// not a second turning point -- keeping it would only re-propose the path step 2
// already produced.  Path order, capped, so the walk stays reproducible.
func residualVs(path []int, onA map[int]bool, source, target, u, cap int) []int {
	var out []int
	seen := map[int]bool{}
	for _, w := range path {
		if w == source || w == target || w == u || onA[w] || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
		if len(out) >= cap {
			break
		}
	}
	return out
}

// joinPaths concatenates an S->u walk with a u->T walk, keeping u once.
func joinPaths(p, q []int) []int {
	out := make([]int, 0, len(p)+len(q))
	out = append(out, p...)
	if len(q) > 0 {
		out = append(out, q[1:]...)
	}
	return out
}

// pathFrom reconstructs the node walk source->target from a *reverse* distance
// array (dt[x] = shortest x -> target under the same blocked set).  It is
// pathTo's mirror image: pathTo walks backwards from the target through Ins
// with a forward distance array, this walks forwards from the source through
// Outs with a reverse one.  Same exact-`==` convention, same determinism.
func pathFrom(g *graph.Graph, dt []float64, source, target int) []int {
	if math.IsInf(dt[source], 1) {
		return nil
	}
	out := make([]int, 0, 8)
	node := source
	for node != target {
		if len(out) > g.N {
			return nil // no tight successor chain: dt and topology disagree
		}
		best := -1
		for _, a := range g.Outs[node] {
			// dt[node] was relaxed as dt[head] + Metric[a], so this comparison
			// is bit-exact (float addition commutes; it does not associate, and
			// this grouping is the one Dijkstra used).
			if dt[node] == g.Metric[a]+dt[g.To[a]] {
				best = a
				break
			}
		}
		if best < 0 {
			return nil
		}
		out = append(out, node)
		node = g.To[best]
	}
	return append(out, target)
}

// pathTo reconstructs the node sequence of a shortest source->target path from
// a forward distance array.  Predecessors are taken in ascending arc id (the
// graph's adjacency order), so the walk is deterministic.
func pathTo(g *graph.Graph, dist []float64, source, target int) []int {
	rev := make([]int, 0, g.N)
	node := target
	for node != source {
		if len(rev) > g.N {
			return nil // no predecessor chain: dist and topology disagree
		}
		best := -1
		for _, a := range g.Ins[node] {
			from := g.From[a]
			// Exact tight-arc test, same convention as crossedHotArcs.
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
	rev = append(rev, source)
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev
}
