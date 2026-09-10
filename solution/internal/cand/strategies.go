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
type stratPool struct {
	nodes []Node
	pairs [][2]int
}

// allPairs returns every unordered pair of ids, in lexicographic order.
func allPairs(ids []int) [][2]int {
	out := make([][2]int, 0, len(ids)*(len(ids)-1)/2)
	for i := 0; i < len(ids); i++ {
		for j := i + 1; j < len(ids); j++ {
			out = append(out, [2]int{ids[i], ids[j]})
		}
	}
	return out
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
// two endpoints of the round's hot arc, then keep only the nodes that provably
// offload a hot arc as pairing material.
//
// The ranking is a *preference*, not a judgement: because it deliberately
// ignores flow direction, a node that sits geometrically close to the hot arc
// but downstream of it can be ranked in while being useless.  The offload test
// (step 5) is what actually removes those -- one real routing per pooled node,
// which is affordable precisely because the pool is capped at PoolCap instead
// of n-2.  Keeping the two roles separate is the point of the strategy; do not
// let the ranking start deciding, or the filter start ranking.
func hotCenter(memo *routeMemo, hp Hops, g *graph.Graph,
	inst *model.Instance, fam Family, opts Options) stratPool {
	dem := &inst.Demands[fam.D]
	var perSlot [][]Node
	pairSeen := map[int]bool{}
	var pair []int

	for _, t := range fam.Slots {
		ball := hopBall(hp, dem, t, opts.PoolCap, opts.MaxExtraHop)
		if len(ball) == 0 {
			continue
		}
		rankByHotDistance(hp, g, t, fam.Hots[t], ball)

		// Step 5: only nodes that really unload a hot arc may generate
		// 2-waypoint candidates.  This is the sole difference between
		// hot_center and od_scan.
		for _, n := range ball {
			if pairSeen[n.ID] {
				continue
			}
			u, ok := memo.at(t, []int{n.ID})
			if !ok {
				continue
			}
			if fam.Relief(t, u) > FracEps {
				pairSeen[n.ID] = true
				pair = append(pair, n.ID)
			}
		}
		for i := range ball {
			ball[i].Tag = ModeHotCenter
		}
		perSlot = append(perSlot, ball)
	}

	nodes := interleave(perSlot, opts.PoolCap)
	return stratPool{nodes: nodes, pairs: allPairs(pair)}
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
// BFS per hot-arc endpoint and no offload test.
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
	nodes := interleave(perSlot, opts.PoolCap)
	ids := make([]int, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	return stratPool{nodes: nodes, pairs: allPairs(ids)}
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
