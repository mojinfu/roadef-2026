package ecmp

import (
	"math"
	"testing"

	"tasr/internal/graph"
)

// tinyGraph:
//
//	nodes 0..3
//	arc 0: 0->1 m=1      arc 1: 0->2 m=1
//	arc 2: 1->3 m=1      arc 3: 2->3 m=1
//	arc 4: 1->2 m=100    (long way around, never tight)
//
// Two equal-cost paths 0->3 exist: 0-1-3 and 0-2-3.
func tinyGraph() *graph.Graph {
	return &graph.Graph{
		N: 4, M: 5,
		From:   []int{0, 0, 1, 2, 1},
		To:     []int{1, 2, 3, 3, 2},
		Metric: []float64{1, 1, 1, 1, 100},
		Cap:    make([]float64, 5),
		Outs:   [][]int{{0, 1}, {2, 4}, {3}, {}},
		Ins:    [][]int{{}, {0}, {1, 4}, {2, 3}},
	}
}

func TestComputeAtomSplitsEvenly(t *testing.T) {
	g := tinyGraph()
	atom := ComputeAtom(g, 0, 3, nil)
	if atom == nil {
		t.Fatal("expected connected atom, got nil")
	}
	want := []float64{0.5, 0.5, 0.5, 0.5, 0}
	for i, w := range want {
		if atom[i] != w {
			t.Errorf("atom[%d] = %v, want %v", i, atom[i], w)
		}
	}
}

func TestComputeAtomDisconnected(t *testing.T) {
	g := tinyGraph()
	// Block the two tight arcs leaving node 0 -> 0 and 3 are disconnected.
	blocked := []bool{true, true, false, false, false}
	atom := ComputeAtom(g, 0, 3, blocked)
	if atom != nil {
		t.Fatalf("expected nil (disconnected), got %v", atom)
	}
}

func TestComputeAtomSelf(t *testing.T) {
	g := tinyGraph()
	atom := ComputeAtom(g, 2, 2, nil)
	if atom == nil || len(atom) != 5 {
		t.Fatalf("u==v should give a zero vector of length M, got %v", atom)
	}
	for i, v := range atom {
		if v != 0 {
			t.Errorf("atom[%d] = %v, want 0", i, v)
		}
	}
}

func TestReverseDijkstraBlocked(t *testing.T) {
	g := tinyGraph()
	blocked := []bool{true, true, false, false, false}
	dist := graph.Dijkstra(g, 3, blocked, true)
	if !math.IsInf(dist[0], 1) {
		t.Fatalf("node 0 should be unreachable, got %v", dist[0])
	}
	if dist[1] != 1 || dist[2] != 1 {
		t.Fatalf("dist[1],dist[2] = %v,%v; want 1,1", dist[1], dist[2])
	}
}
