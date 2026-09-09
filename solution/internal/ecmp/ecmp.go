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
package ecmp

import (
	"container/heap"
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
	maxSize int

	memo map[uvt]*list.Element
	lru  *list.List
}

type uvt struct{ u, v, t int }

type cacheEntry struct {
	key  uvt
	vals []float64 // nil => disconnected
}

// NewCache builds a cache keyed on the scenario's per-slot down-arc sets.
// maxSize <= 0 means unlimited.
func NewCache(g *graph.Graph, blocked [][]bool, maxSize int) *Cache {
	if maxSize <= 0 {
		maxSize = 1 << 30
	}
	return &Cache{
		g:       g,
		blocked: blocked,
		maxSize: maxSize,
		memo:    make(map[uvt]*list.Element),
		lru:     list.New(),
	}
}

// Atom returns the unit-flow split vector for segment (u, v) at slot t, or nil
// when u and v are disconnected once the slot's down arcs are removed.
func (c *Cache) Atom(u, v, t int) []float64 {
	if t < 0 || t >= len(c.blocked) {
		panic(fmt.Sprintf("slot %d out of range [0,%d)", t, len(c.blocked)))
	}
	key := uvt{u, v, t}
	if el, ok := c.memo[key]; ok {
		c.lru.MoveToFront(el)
		return el.Value.(*cacheEntry).vals
	}
	vals := ComputeAtom(c.g, u, v, c.blocked[t])
	ent := &cacheEntry{key: key, vals: vals}
	c.memo[key] = c.lru.PushFront(ent)
	if c.lru.Len() > c.maxSize {
		back := c.lru.Back()
		if back != nil {
			c.lru.Remove(back)
			delete(c.memo, back.Value.(*cacheEntry).key)
		}
	}
	return vals
}

// Clear drops all cached atoms.
func (c *Cache) Clear() {
	c.memo = make(map[uvt]*list.Element)
	c.lru = list.New()
}

// ComputeAtom is the bare atom computation (no caching).
func ComputeAtom(g *graph.Graph, u, v int, blocked []bool) []float64 {
	if u == v {
		return make([]float64, g.M)
	}
	dist := dijkstraDist(g, v, blocked, true)
	if math.IsInf(dist[u], 1) {
		return nil
	}

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

// dijkstraDist computes shortest distances from source, skipping blocked arcs.
// reverse=false -> dist[x] = source -> x; reverse=true -> dist[x] = x -> source
// (Dijkstra run over reversed adjacency).  Relaxations happen in exactly the
// reference order: adjacency arcs in ascending id, nd = d + metric, strict <.
func dijkstraDist(g *graph.Graph, source int, blocked []bool, reverse bool) []float64 {
	dist := make([]float64, g.N)
	for i := range dist {
		dist[i] = math.Inf(1)
	}
	dist[source] = 0.0
	adj := g.Outs
	if reverse {
		adj = g.Ins
	}

	h := &nodeHeap{{dist: 0.0, node: source}}
	for h.Len() > 0 {
		it := heap.Pop(h).(pair)
		d, node := it.dist, it.node
		if d > dist[node] {
			continue
		}
		for _, aid := range adj[node] {
			if blocked != nil && blocked[aid] {
				continue
			}
			var nxt int
			if reverse {
				nxt = g.From[aid]
			} else {
				nxt = g.To[aid]
			}
			nd := d + g.Metric[aid]
			if nd < dist[nxt] {
				dist[nxt] = nd
				heap.Push(h, pair{dist: nd, node: nxt})
			}
		}
	}
	return dist
}

// DistancesFrom is a convenience for the solver: shortest u -> * distances.
func DistancesFrom(g *graph.Graph, source int, blocked []bool) []float64 {
	return dijkstraDist(g, source, blocked, false)
}

// DistancesTo is a convenience for the solver: shortest * -> v distances.
func DistancesTo(g *graph.Graph, target int, blocked []bool) []float64 {
	return dijkstraDist(g, target, blocked, true)
}

type pair struct {
	dist float64
	node int
}

type nodeHeap []pair

func (h nodeHeap) Len() int { return len(h) }
func (h nodeHeap) Less(i, j int) bool {
	if h[i].dist != h[j].dist {
		return h[i].dist < h[j].dist
	}
	return h[i].node < h[j].node
}
func (h nodeHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *nodeHeap) Push(x interface{}) {
	*h = append(*h, x.(pair))
}
func (h *nodeHeap) Pop() interface{} {
	old := *h
	n := len(old)
	it := old[n-1]
	*h = old[:n-1]
	return it
}
