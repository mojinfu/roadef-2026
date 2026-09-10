// Structural primitives of the presolve stage: directed reachability with one
// extra arc banned, and the undirected cut edges ("bridges") of the topology.
//
// All four functions are pure: they take the Graph plus a per-slot blocked mask
// (the same `blocked []bool` convention as ecmp and search.go) and hold no
// state.  Nothing here is cached -- presolve runs once per solve, so caching it
// would only pollute the shared ECMP/index LRUs.
//
// The topology is stored as directed arcs with (in every challenge instance
// seen so far) one arc per direction.  An undirected *edge* is therefore a pair
// {u,v} that is usable while at least one of its two arcs is not blocked.
// A bridge edge is the only connection between the two sides, so any demand
// whose endpoints fall on opposite sides must traverse one of the edge's (at
// most two) arcs -- that is the class-1 forced-load argument of the presolve.
package graph

import "sort"

// OutDegreeAvoiding returns how many arcs leave node when both the slot's
// blocked mask and one extra arc (ban, <0 for none) are removed.
func OutDegreeAvoiding(g *Graph, blocked []bool, node, ban int) int {
	if node < 0 || node >= g.N {
		return 0
	}
	n := 0
	for _, aid := range g.Outs[node] {
		if blocked != nil && blocked[aid] {
			continue
		}
		if aid == ban {
			continue
		}
		n++
	}
	return n
}

// InDegreeAvoiding is the mirror of OutDegreeAvoiding.
func InDegreeAvoiding(g *Graph, blocked []bool, node, ban int) int {
	if node < 0 || node >= g.N {
		return 0
	}
	n := 0
	for _, aid := range g.Ins[node] {
		if blocked != nil && blocked[aid] {
			continue
		}
		if aid == ban {
			continue
		}
		n++
	}
	return n
}

// ReachableAvoiding reports whether target can be reached from source while the
// slot's blocked mask and one extra arc (ban, <0 for none) are removed.  Plain
// unweighted BFS over the forward adjacency: reachability, not distance, is
// what the class-1 forced-load test needs.
func ReachableAvoiding(g *Graph, blocked []bool, source, target, ban int) bool {
	if source == target {
		return true
	}
	seen := make([]bool, g.N)
	seen[source] = true
	queue := []int{source}
	for len(queue) > 0 {
		x := queue[0]
		queue = queue[1:]
		for _, aid := range g.Outs[x] {
			if blocked != nil && blocked[aid] || aid == ban {
				continue
			}
			y := g.To[aid]
			if y == target {
				return true
			}
			if !seen[y] {
				seen[y] = true
				queue = append(queue, y)
			}
		}
	}
	return false
}

// Bridge is one cut edge of the undirected topology.  U < V; ArcUV / ArcVU are
// the arc ids of the two directions, or -1 when the direction is absent or
// blocked at the queried slot.
type Bridge struct {
	U, V  int
	ArcUV int
	ArcVU int
}

// Bridges returns every cut edge of the undirected topology under the given
// blocked mask, sorted by (U, V) for determinism.
//
// Iterative Tarjan lowlink over the pair graph.  The recursion is unrolled
// because instance sizes are unbounded (setB-01 already has n=264, and a
// recursive DFS over a chain topology would grow the stack with n).
func Bridges(g *Graph, blocked []bool) []Bridge {
	type dirs struct{ uv, vu int }
	pairs := map[int]dirs{}
	for aid := 0; aid < g.M; aid++ {
		if blocked != nil && blocked[aid] {
			continue
		}
		u, v := g.From[aid], g.To[aid]
		if u == v {
			continue // self-loops never affect connectivity
		}
		lo, hi := u, v
		if lo > hi {
			lo, hi = hi, lo
		}
		d := pairs[lo*g.N+hi]
		if u == lo {
			d.uv = aid
		} else {
			d.vu = aid
		}
		pairs[lo*g.N+hi] = d
	}

	edges := make([]Bridge, 0, len(pairs))
	for k, d := range pairs {
		edges = append(edges, Bridge{U: k / g.N, V: k % g.N, ArcUV: d.uv, ArcVU: d.vu})
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].U != edges[j].U {
			return edges[i].U < edges[j].U
		}
		return edges[i].V < edges[j].V
	})

	type half struct {
		to   int
		edge int
	}
	adj := make([][]half, g.N)
	for ei := range edges {
		e := &edges[ei]
		if e.ArcUV < 0 && e.ArcVU < 0 {
			continue // both directions absent or blocked: not traversable at all
		}
		adj[e.U] = append(adj[e.U], half{to: e.V, edge: ei})
		adj[e.V] = append(adj[e.V], half{to: e.U, edge: ei})
	}

	disc := make([]int, g.N)
	low := make([]int, g.N)
	for i := range disc {
		disc[i] = -1
	}
	timer := 0
	type frame struct {
		node   int
		via    int
		cursor int
	}
	var out []Bridge
	for root := 0; root < g.N; root++ {
		if disc[root] != -1 {
			continue
		}
		disc[root], low[root] = timer, timer
		timer++
		stack := []frame{{node: root, via: -1}}
		for len(stack) > 0 {
			fr := &stack[len(stack)-1]
			if fr.cursor < len(adj[fr.node]) {
				h := adj[fr.node][fr.cursor]
				fr.cursor++
				if h.edge == fr.via {
					continue // do not walk straight back over the tree edge
				}
				if disc[h.to] == -1 {
					disc[h.to], low[h.to] = timer, timer
					timer++
					stack = append(stack, frame{node: h.to, via: h.edge})
					continue
				}
				if disc[h.to] < low[fr.node] {
					low[fr.node] = disc[h.to]
				}
				continue
			}
			child := fr.node
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				continue
			}
			parent := &stack[len(stack)-1]
			if low[child] < low[parent.node] {
				low[parent.node] = low[child]
			}
			if low[child] > disc[parent.node] {
				out = append(out, edges[fr.via])
			}
		}
	}
	return out
}

// BridgeSide returns the membership mask of the side of bridge edge {u,v} that
// contains u, computed on the topology with that edge removed (both directions)
// and the slot's blocked mask applied.  Traversal follows the pair graph, i.e.
// either direction of every other edge.
func BridgeSide(g *Graph, blocked []bool, u, v int) []bool {
	side := make([]bool, g.N)
	if u < 0 || u >= g.N {
		return side
	}
	crossing := func(aid int) bool {
		return (g.From[aid] == u && g.To[aid] == v) || (g.From[aid] == v && g.To[aid] == u)
	}
	side[u] = true
	queue := []int{u}
	for len(queue) > 0 {
		x := queue[0]
		queue = queue[1:]
		for _, aid := range g.Outs[x] {
			if blocked != nil && blocked[aid] || crossing(aid) {
				continue
			}
			if y := g.To[aid]; !side[y] {
				side[y] = true
				queue = append(queue, y)
			}
		}
		for _, aid := range g.Ins[x] {
			if blocked != nil && blocked[aid] || crossing(aid) {
				continue
			}
			if y := g.From[aid]; !side[y] {
				side[y] = true
				queue = append(queue, y)
			}
		}
	}
	return side
}
