// Single-source graph queries over a banned-arc set.
//
// These are the raw primitives of the graph-acceleration layer: an exact
// metric Dijkstra (relaxations in the checker-aligned reference order) and an
// unweighted hop BFS.  Both accept the same `blocked` convention used by the
// ECMP atom code (nil means nothing is down; a true entry bans that arc id),
// and both can run forward (source -> nodes) or over reversed adjacency
// (nodes -> source).
package graph

import (
	"container/heap"
	"math"
)

// Dijkstra computes shortest distances from source to every node, skipping
// arcs for which blocked is non-nil and true.
//
// reverse=false -> dist[x] = shortest source -> x; reverse=true -> dist[x] =
// shortest x -> source (Dijkstra run over the reversed adjacency).  Each
// relaxation d + metric is done in exactly the reference order used by the
// checker-aligned ECMP code: adjacency arcs in ascending arc id, strict `<`
// improvement, heap ties broken by node index.  Moving this out of the ecmp
// package does not change any distance or split coefficient; the caller that
// needs stability must not mutate the returned slice.
func Dijkstra(g *Graph, source int, blocked []bool, reverse bool) []float64 {
	dist := make([]float64, g.N)
	DijkstraInto(g, source, blocked, reverse, dist)
	return dist
}

// DijkstraInto is Dijkstra into a caller-provided buffer of length >= g.N,
// which is overwritten.  Presolve's hot loops reuse one buffer to avoid an
// allocation (and the GC churn) per distance computation.
func DijkstraInto(g *Graph, source int, blocked []bool, reverse bool, dist []float64) {
	for i := 0; i < g.N; i++ {
		dist[i] = math.Inf(1)
	}
	dist[source] = 0.0
	adj := g.Outs
	if reverse {
		adj = g.Ins
	}

	h := &spNodeHeap{{dist: 0.0, node: source}}
	for h.Len() > 0 {
		it := heap.Pop(h).(spPair)
		d, node := it.dist, it.node
		if d > dist[node] {
			continue
		}
		for _, aid := range adj[node] {
			if blocked != nil && blocked[aid] {
				continue
			}
			nxt := g.To[aid]
			if reverse {
				nxt = g.From[aid]
			}
			nd := d + g.Metric[aid]
			if nd < dist[nxt] {
				dist[nxt] = nd
				heap.Push(h, spPair{dist: nd, node: nxt})
			}
		}
	}
}

// HopCounts computes, per node, the minimum number of arcs on a path from
// source that avoids blocked arcs (reverse=true: from the node back to
// source), or -1 when the node is unreachable.  All arcs count as one hop, so
// this is a plain BFS over the unweighted residual network.
func HopCounts(g *Graph, source int, blocked []bool, reverse bool) []int {
	hops := make([]int, g.N)
	for i := range hops {
		hops[i] = -1
	}
	hops[source] = 0
	queue := []int{source}
	adj := g.Outs
	if reverse {
		adj = g.Ins
	}
	for head := 0; head < len(queue); head++ {
		node := queue[head]
		for _, aid := range adj[node] {
			if blocked != nil && blocked[aid] {
				continue
			}
			nxt := g.To[aid]
			if reverse {
				nxt = g.From[aid]
			}
			if hops[nxt] == -1 {
				hops[nxt] = hops[node] + 1
				queue = append(queue, nxt)
			}
		}
	}
	return hops
}

// HopCountsUndirected computes, per node, the minimum number of arcs on a path
// from source that avoids blocked arcs when every arc may be traversed in
// either direction (adjacency = Outs ∪ Ins).
//
// This is the *undirected proximity* measure, used only to rank candidate
// waypoints by "how close is this node to a hot arc" (design doc §3.1 step 4).
// It is deliberately distinct from HopCounts(reverse=false|true): those are
// directed, so HopsFrom(s)[w] + HopsTo(t)[w] is a directed sum that only means
// something once a specific source/target pair is fixed.  The undirected
// variant is symmetric (d(u,w) == d(w,u)) and carries no flow direction, which
// is what a geometric nearness ranking wants.
//
// Only ranking may use this.  No feasibility test, filter or routing decision
// may depend on it -- the offload test (design doc §3.1 step 5) is the sole
// authority on whether a node is useful, and it routes for real.
func HopCountsUndirected(g *Graph, source int, blocked []bool) []int {
	hops := make([]int, g.N)
	for i := range hops {
		hops[i] = -1
	}
	hops[source] = 0
	queue := []int{source}
	for head := 0; head < len(queue); head++ {
		node := queue[head]
		for _, aid := range g.Outs[node] {
			if blocked != nil && blocked[aid] {
				continue
			}
			if nxt := g.To[aid]; hops[nxt] == -1 {
				hops[nxt] = hops[node] + 1
				queue = append(queue, nxt)
			}
		}
		for _, aid := range g.Ins[node] {
			if blocked != nil && blocked[aid] {
				continue
			}
			if nxt := g.From[aid]; hops[nxt] == -1 {
				hops[nxt] = hops[node] + 1
				queue = append(queue, nxt)
			}
		}
	}
	return hops
}

type spPair struct {
	dist float64
	node int
}

type spNodeHeap []spPair

func (h spNodeHeap) Len() int { return len(h) }
func (h spNodeHeap) Less(i, j int) bool {
	if h[i].dist != h[j].dist {
		return h[i].dist < h[j].dist
	}
	return h[i].node < h[j].node
}
func (h spNodeHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *spNodeHeap) Push(x interface{}) {
	*h = append(*h, x.(spPair))
}
func (h *spNodeHeap) Pop() interface{} {
	old := *h
	n := len(old)
	it := old[n-1]
	*h = old[:n-1]
	return it
}
