package graph

import (
	"math"
	"testing"
)

// tinyGraph matches the ecmp test fixture:
//
//	nodes 0..3
//	arc 0: 0->1 m=1      arc 1: 0->2 m=1
//	arc 2: 1->3 m=1      arc 3: 2->3 m=1
//	arc 4: 1->2 m=100    (long way around, never tight)
func tinyGraph() *Graph {
	return &Graph{
		N: 4, M: 5,
		From:   []int{0, 0, 1, 2, 1},
		To:     []int{1, 2, 3, 3, 2},
		Metric: []float64{1, 1, 1, 1, 100},
		Cap:    make([]float64, 5),
		Outs:   [][]int{{0, 1}, {2, 4}, {3}, {}},
		Ins:    [][]int{{}, {0}, {1, 4}, {2, 3}},
	}
}

func TestDijkstraForward(t *testing.T) {
	g := tinyGraph()
	dist := Dijkstra(g, 0, nil, false)
	want := []float64{0, 1, 1, 2}
	for i, w := range want {
		if dist[i] != w {
			t.Errorf("dist[%d]=%v, want %v", i, dist[i], w)
		}
	}
}

func TestDijkstraReverseTiesBackward(t *testing.T) {
	// dist_r[x] = shortest x -> 3 under the long arc 4 (1->2) never used.
	g := tinyGraph()
	dist := Dijkstra(g, 3, nil, true)
	want := []float64{2, 1, 1, 0}
	for i, w := range want {
		if dist[i] != w {
			t.Errorf("dist[%d]=%v, want %v", i, dist[i], w)
		}
	}
}

func TestDijkstraBlockedDisconnects(t *testing.T) {
	g := tinyGraph()
	blocked := []bool{true, true, false, false, false} // cut 0's two exits
	dist := Dijkstra(g, 3, blocked, true)
	if !math.IsInf(dist[0], 1) {
		t.Fatalf("node 0 should be unreachable from 3, got %v", dist[0])
	}
	if dist[1] != 1 || dist[2] != 1 || dist[3] != 0 {
		t.Fatalf("dist[1..3]=%v,%v,%v, want 1,1,0", dist[1], dist[2], dist[3])
	}
}

func TestHopCounts(t *testing.T) {
	g := tinyGraph()
	// hops 0->3: 0-1-3 or 0-2-3, both 2 arcs; long arc is 100 so still one hop.
	hops := HopCounts(g, 0, nil, false)
	want := []int{0, 1, 1, 2}
	for i, w := range want {
		if hops[i] != w {
			t.Errorf("hops[%d]=%d, want %d", i, hops[i], w)
		}
	}
	// 3 -> 0 blocked (reverse): same shape.
	blocked := []bool{true, true, false, false, false}
	hopsR := HopCounts(g, 3, blocked, true)
	if hopsR[0] != -1 {
		t.Fatalf("hop 3->0 should be unreachable, got %d", hopsR[0])
	}
}

func TestHopCountsUnreachable(t *testing.T) {
	// Two-component graph.
	g := &Graph{
		N: 4, M: 2,
		From:   []int{0, 2},
		To:     []int{1, 3},
		Metric: []float64{1, 1},
		Cap:    make([]float64, 2),
		Outs:   [][]int{{0}, {}, {1}, {}},
		Ins:    [][]int{{}, {0}, {}, {1}},
	}
	hops := HopCounts(g, 0, nil, false)
	if hops[2] != -1 || hops[3] != -1 {
		t.Fatalf("component 2,3 should be unreachable, got %v", hops)
	}
}

func TestIndexDistCachesSharedSlice(t *testing.T) {
	g := tinyGraph()
	blocked := make([][]bool, 1) // slot 0: nothing down
	ix := NewIndex(g, blocked, 0)

	d1 := ix.DistTo(0, 3)
	d2 := ix.DistTo(0, 3) // cache hit, must be the same array
	if &d1[0] != &d2[0] {
		t.Fatal("DistTo should return the shared cached slice")
	}
	if d1[0] != 2 {
		t.Fatalf("dist 0->3 = %v, want 2", d1[0])
	}

	df := ix.DistFrom(0, 0)
	if df[3] != 2 {
		t.Fatalf("DistFrom 0->3 = %v, want 2", df[3])
	}
	// DistFrom must not alias DistTo.
	if &d1[0] == &df[0] {
		t.Fatal("dist arrays of different queries must not alias")
	}
}

func TestIndexHops(t *testing.T) {
	g := tinyGraph()
	blocked := make([][]bool, 2)
	blocked[1] = []bool{true, true, false, false, false}
	ix := NewIndex(g, blocked, 0)

	h := ix.HopsFrom(1, 0)
	if h[0] != 0 || h[3] != -1 {
		t.Fatalf("slot1 hops 0->3 = %v (want -1 at 3), got %v", h, h)
	}
	// Slot 0 unchanged.
	h0 := ix.HopsTo(0, 3)
	if h0[0] != 2 {
		t.Fatalf("slot0 hops 3->0 = %d, want 2", h0[0])
	}
}

func TestIndexLRUEviction(t *testing.T) {
	g := tinyGraph()
	blocked := make([][]bool, 1)
	ix := NewIndex(g, blocked, 2) // tiny bound

	a := ix.DistTo(0, 0)
	b := ix.DistTo(0, 1)
	c := ix.DistTo(0, 2) // evicts a (LRU)
	_ = a
	_ = b
	_ = c
	if len(ix.memo) > 2 {
		t.Fatalf("LRU bound broken: %d entries", len(ix.memo))
	}
	// Evicted entry must still compute correctly on re-query.
	d := ix.DistFrom(0, 0)
	if d[1] != 1 {
		t.Fatalf("recomputed dist[1] = %v, want 1", d[1])
	}
}
