// Package ecmp computes the ECMP forwarding split coefficients
// r(u, v, a, t): the fraction of a unit demand from u to v that crosses each
// arc, at time slot t (arcs down by an intervention at t are removed).
//
// The algorithm replicates bit-for-bit the reference Python implementation in
// solution/_pyref/tasr/ecmp/atoms.py, which was validated 40/40 against the
// official checker:
//
//   - a *single* backward Dijkstra from v yields dist_r[x] = shortest x -> v
//     (metrics are relaxed as d + metric in the same order, heap ties broken by
//     node index like heapq);
//   - an arc x -> y belongs to the forwarding graph iff
//     dist_r[x] == dist_r[y] + metric(x, y) with **exact float equality**;
//   - nodes are processed from the source side outwards (decreasing dist_r),
//     flow entering a node being split evenly among its tight outgoing arcs.
//
// The backward Dijkstra itself lives in internal/graph (graph.Dijkstra); ecmp
// only supplies the split that turns a distance array into an atom vector.
// Distances are pulled through the shared per-(slot, banned-set) index, so a
// segment head v that many candidates share costs one Dijkstra per (v, slot),
// not one per (u, v, slot).
package ecmp

import (
	"container/list"
	"fmt"
	"math"
	"sort"

	"tasr/internal/graph"
)

// Cache memoises atom vectors per (u, v, t).  Vectors are dense []float64 of
// length M and must not be mutated by callers.  A cached nil means the segment
// (u, v) is disconnected at slot t.
type Cache struct {
	g       *graph.Graph
	blocked [][]bool // per slot: blocked[arcID]
	idx     *graph.Index
	maxSize int

	memo map[uvt]*list.Element
	lru  *list.List

	// Counters, for reporting only: they never feed a decision.  Hits/Misses
	// count atom lookups and Evicted counts entries the LRU dropped, which is
	// what says whether a bound is thrashing rather than merely working.
	Hits    int
	Misses  int
	Evicted int
}

type uvt struct{ u, v, t int }

type cacheEntry struct {
	key  uvt
	vals []float64 // nil => disconnected
}

// NewCache builds a cache keyed on the scenario's per-slot down-arc sets.
// maxSize <= 0 means effectively unlimited (1<<30), which is fine for the
// one-shot tools but NOT for the round loop: the key is (u, v, t) over
// *arbitrary* node pairs -- UnitRoute asks for one segment per leg of a route,
// so waypoints, not arcs, decide the pairs -- and every accepted round re-routes
// the whole incumbent, so the key space is n^2*T and a long run retains every
// pair it ever touched.  cmd/solve sizes this bound; a caller that loops must
// do the same (see the atomCap block there for the measured sizes).
// The attached graph.Index is bounded internally (LRU), independently of the
// atom memo.
func NewCache(g *graph.Graph, blocked [][]bool, maxSize int) *Cache {
	if maxSize <= 0 {
		maxSize = 1 << 30
	}
	return &Cache{
		g:       g,
		blocked: blocked,
		idx:     graph.NewIndex(g, blocked, 0),
		maxSize: maxSize,
		memo:    make(map[uvt]*list.Element),
		lru:     list.New(),
	}
}

// Index returns the graph acceleration index this cache computes through.  The
// atom path pulls its reverse distances from it (atom -> idx.DistTo), so it is
// already warm: callers that need single-source queries (the candidate layer)
// must reuse this exact instance.  Building a second graph.Index would repeat
// every BFS/Dijkstra the atom code already paid for and double the LRU
// footprint.  Returned arrays are shared with the cache and must not be
// mutated.
func (c *Cache) Index() *graph.Index { return c.idx }

// Atom returns the unit-flow split vector for segment (u, v) at slot t, or nil
// when u and v are disconnected once the slot's down arcs are removed.
func (c *Cache) Atom(u, v, t int) []float64 {
	if t < 0 || t >= len(c.blocked) {
		panic(fmt.Sprintf("slot %d out of range [0,%d)", t, len(c.blocked)))
	}
	key := uvt{u, v, t}
	if el, ok := c.memo[key]; ok {
		c.Hits++
		c.lru.MoveToFront(el)
		return el.Value.(*cacheEntry).vals
	}
	c.Misses++
	vals := c.atom(u, v, t)
	ent := &cacheEntry{key: key, vals: vals}
	c.memo[key] = c.lru.PushFront(ent)
	if c.lru.Len() > c.maxSize {
		back := c.lru.Back()
		if back != nil {
			c.lru.Remove(back)
			delete(c.memo, back.Value.(*cacheEntry).key)
			c.Evicted++
		}
	}
	return vals
}

// atom computes the vector for (u, v, t) with no memoisation, reusing the
// slot's cached reverse distances to v.
func (c *Cache) atom(u, v, t int) []float64 {
	if u == v {
		return make([]float64, c.g.M)
	}
	dist := c.idx.DistTo(t, v)
	if math.IsInf(dist[u], 1) {
		return nil
	}
	return splitAtom(c.g, u, v, dist, c.blocked[t])
}

// Clear drops all cached atoms (the distance index is kept).
func (c *Cache) Clear() {
	c.memo = make(map[uvt]*list.Element)
	c.lru = list.New()
}

// ComputeAtom is the bare atom computation (no caching): a fresh reverse
// Dijkstra from v, then splitAtom.  Used by tests and one-shot callers; the
// Cache path goes through atom() instead so distances are shared.
func ComputeAtom(g *graph.Graph, u, v int, blocked []bool) []float64 {
	if u == v {
		return make([]float64, g.M)
	}
	dist := graph.Dijkstra(g, v, blocked, true)
	if math.IsInf(dist[u], 1) {
		return nil
	}
	return splitAtom(g, u, v, dist, blocked)
}

// splitAtom spreads one unit of flow from u to v across the tight arcs implied
// by dist (shortest x -> v distances under the same blocked set).  dist is
// read-only and shared when it comes from the index.
func splitAtom(g *graph.Graph, u, v int, dist []float64, blocked []bool) []float64 {
	atom := make([]float64, g.M)
	flow := make([]float64, g.N)
	flow[u] = 1.0

	// Nodes with finite dist, decreasing dist_r.  Sort is stable so ties keep
	// ascending node order exactly like Python's stable sort over range(n).
	order := make([]int, 0, g.N)
	for node, d := range dist {
		if !math.IsInf(d, 1) {
			order = append(order, node)
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		return dist[order[i]] > dist[order[j]]
	})

	for _, node := range order {
		fNode := flow[node]
		if fNode == 0.0 {
			continue
		}
		var tight []int
		for _, aid := range g.Outs[node] {
			if blocked != nil && blocked[aid] {
				continue
			}
			// x -> y is on a shortest x->v path iff it was an exact-tie
			// predecessor of the backward Dijkstra.  No tolerance: this mirrors
			// networktools' `==` relaxation and matches the checker.
			if dist[node] == dist[g.To[aid]]+g.Metric[aid] {
				tight = append(tight, aid)
			}
		}
		if len(tight) == 0 {
			if node != v {
				panic(fmt.Sprintf("ECMP flow stalled at node %d (u=%d, v=%d)", node, u, v))
			}
			continue
		}
		share := fNode / float64(len(tight))
		for _, aid := range tight {
			atom[aid] += share
			flow[g.To[aid]] += share
		}
	}
	return atom
}

// DistancesFrom is a convenience for the solver: shortest u -> * distances.
func DistancesFrom(g *graph.Graph, source int, blocked []bool) []float64 {
	return graph.Dijkstra(g, source, blocked, false)
}

// DistancesTo is a convenience for the solver: shortest * -> v distances.
func DistancesTo(g *graph.Graph, target int, blocked []bool) []float64 {
	return graph.Dijkstra(g, target, blocked, true)
}
