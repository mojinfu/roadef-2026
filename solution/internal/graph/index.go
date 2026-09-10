// Lazy per-(slot, banned-set) query cache, the shared acceleration layer the
// solver builds once per instance and attaches to the ECMP atom cache.
//
// The physical topology and the per-slot down-arc sets never change during a
// solve, so any single-source metric distance or hop-count array is a pure
// function of (slot t, source).  Computing them on demand and memoising avoids
// the repeated backward Dijkstras the decompose loop would otherwise pay once
// per candidate waypoint: for a fixed segment head v at slot t, every
// candidate u that ends at v shares the same reverse distance array.
//
// Five independent result kinds are cached:
//
//	kindDistTo   dist[x] = shortest x -> v        (reverse Dijkstra from v)
//	kindDistFrom dist[x] = shortest u -> x        (forward Dijkstra from u)
//	kindHopsTo   hops[x] = fewest arcs x -> v
//	kindHopsFrom hops[x] = fewest arcs u -> x
//	kindHopsUnd  hops[x] = fewest arcs u <-> x, arcs usable either way (BFS)
//
// The undirected kind exists only for the candidate layer's proximity ranking
// (cand, design doc §3.1): "how many hops from this waypoint to the hot arc's
// endpoints, ignoring flow direction".  It is not a routing quantity and must
// never be used to decide feasibility.
//
// All entries are stored in one LRU so total memory stays bounded regardless
// of how many sources a long solve touches.  Returned slices are shared with
// the cache and must not be mutated by callers.
package graph

import (
	"container/list"
)

type idxKind uint8

const (
	kindDistTo idxKind = iota
	kindDistFrom
	kindHopsTo
	kindHopsFrom
	kindHopsUnd
)

type idxKey struct {
	t, src int
	kind   idxKind
}

type idxEntry struct {
	key  idxKey
	dist []float64 // kindDist*: metric distances, never nil
	hops []int     // kindHops*: hop counts, never nil
}

// Index memoises the single-source query results of one (graph, per-slot
// banned-arc sets) pair.  Not safe for concurrent use (mirrors Cache).
type Index struct {
	g       *Graph
	blocked [][]bool
	maxSize int

	memo map[idxKey]*list.Element
	lru  *list.List
}

// NewIndex builds an index over g and the scenario's per-slot blocked sets.
// maxSize bounds the number of cached arrays (0 -> default 2048).
func NewIndex(g *Graph, blocked [][]bool, maxSize int) *Index {
	if maxSize <= 0 {
		maxSize = 2048
	}
	return &Index{
		g:       g,
		blocked: blocked,
		maxSize: maxSize,
		memo:    make(map[idxKey]*list.Element),
		lru:     list.New(),
	}
}

func (ix *Index) get(k idxKey) (*idxEntry, bool) {
	if el, ok := ix.memo[k]; ok {
		ix.lru.MoveToFront(el)
		return el.Value.(*idxEntry), true
	}
	return nil, false
}

func (ix *Index) put(e *idxEntry) {
	ix.memo[e.key] = ix.lru.PushFront(e)
	if ix.lru.Len() > ix.maxSize {
		back := ix.lru.Back()
		if back != nil {
			ix.lru.Remove(back)
			delete(ix.memo, back.Value.(*idxEntry).key)
		}
	}
}

// blockedAt returns the banned set of slot t (nil when t is out of range or
// the scenario has no per-slot block lists).
func (ix *Index) blockedAt(t int) []bool {
	if t < 0 || t >= len(ix.blocked) {
		return nil
	}
	return ix.blocked[t]
}

// DistTo returns dist[x] = shortest metric x -> v with slot t's arcs removed.
func (ix *Index) DistTo(t, v int) []float64 {
	k := idxKey{t: t, src: v, kind: kindDistTo}
	if e, ok := ix.get(k); ok {
		return e.dist
	}
	d := Dijkstra(ix.g, v, ix.blockedAt(t), true)
	ix.put(&idxEntry{key: k, dist: d})
	return d
}

// DistFrom returns dist[x] = shortest metric u -> x with slot t's arcs removed.
func (ix *Index) DistFrom(t, u int) []float64 {
	k := idxKey{t: t, src: u, kind: kindDistFrom}
	if e, ok := ix.get(k); ok {
		return e.dist
	}
	d := Dijkstra(ix.g, u, ix.blockedAt(t), false)
	ix.put(&idxEntry{key: k, dist: d})
	return d
}

// HopsTo returns hops[x] = fewest arcs x -> v (-1 unreachable).
func (ix *Index) HopsTo(t, v int) []int {
	k := idxKey{t: t, src: v, kind: kindHopsTo}
	if e, ok := ix.get(k); ok {
		return e.hops
	}
	h := HopCounts(ix.g, v, ix.blockedAt(t), true)
	ix.put(&idxEntry{key: k, hops: h})
	return h
}

// HopsFrom returns hops[x] = fewest arcs u -> x (-1 unreachable).
func (ix *Index) HopsFrom(t, u int) []int {
	k := idxKey{t: t, src: u, kind: kindHopsFrom}
	if e, ok := ix.get(k); ok {
		return e.hops
	}
	h := HopCounts(ix.g, u, ix.blockedAt(t), false)
	ix.put(&idxEntry{key: k, hops: h})
	return h
}

// HopsUnd returns hops[x] = fewest arcs on a walk u <-> x when every arc may be
// traversed in either direction (-1 unreachable).  Ranking-only primitive; see
// HopCountsUndirected for why it is not HopCounts with a flag.
func (ix *Index) HopsUnd(t, u int) []int {
	k := idxKey{t: t, src: u, kind: kindHopsUnd}
	if e, ok := ix.get(k); ok {
		return e.hops
	}
	h := HopCountsUndirected(ix.g, u, ix.blockedAt(t))
	ix.put(&idxEntry{key: k, hops: h})
	return h
}

// SetMaxSize raises (or lowers) the LRU bound.  The candidate layer issues far
// more distinct (slot, source) queries than the ECMP atom path, so the default
// can make the two evict each other in a hot loop; a caller that knows it will
// run the candidate strategies should raise this once at start-up.  Existing
// entries are kept; the new bound applies from the next insertion.
func (ix *Index) SetMaxSize(n int) {
	if n > 0 {
		ix.maxSize = n
	}
}

// Clear drops all cached arrays (the underlying graph is unchanged).
func (ix *Index) Clear() {
	ix.memo = make(map[idxKey]*list.Element)
	ix.lru = list.New()
}
