// Package graph is a compact adjacency container over an Instance's arcs.
//
// Nodes are 0..n-1 and arc ids equal their index in the From/To/Metric/Cap
// arrays.  Both forward (Outs) and backward (Ins) adjacency are kept so
// Dijkstra can run in either direction (needed for ECMP forwarding graphs).
// Adjacency lists are in ascending arc-id order, exactly like the reference
// Python DirectedGraph.
package graph

import "tasr/internal/model"

type Graph struct {
	N      int
	M      int
	From   []int     // arc id -> tail position
	To     []int     // arc id -> head position
	Metric []float64 // arc id -> metric
	Cap    []float64 // arc id -> capacity
	Outs   [][]int   // node -> arc ids leaving it
	Ins    [][]int   // node -> arc ids entering it
}

// New builds a Graph from an instance.  Arc ids must be 0..m-1 in order.
func New(inst *model.Instance) *Graph {
	n := inst.NNodes()
	m := inst.NArcs()
	g := &Graph{
		N:      n,
		M:      m,
		From:   make([]int, m),
		To:     make([]int, m),
		Metric: make([]float64, m),
		Cap:    make([]float64, m),
		Outs:   make([][]int, n),
		Ins:    make([][]int, n),
	}
	for i, a := range inst.Arcs {
		if a.ID != i {
			panic("arc ids must be contiguous 0..m-1")
		}
		g.From[i] = a.From
		g.To[i] = a.To
		g.Metric[i] = a.Metric
		g.Cap[i] = a.Capacity
	}
	// Outs/Ins filled by scanning arcs in id order (matches Python: each node
	// adjacency list ends up in ascending arc-id order).
	for i, a := range inst.Arcs {
		g.Outs[a.From] = append(g.Outs[a.From], i)
		g.Ins[a.To] = append(g.Ins[a.To], i)
	}
	return g
}
