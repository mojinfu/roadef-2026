// Package eval computes arc saturation matrices by superposition of ECMP atoms
// and implements the challenge's lexicographic objective over the sorted vector
// of saturations truncated to 6 decimal places.
//
// Saturation matrix layout throughout: flat []float64 of length m*T, indexed
// [arc*T + slot] (slot-major output, arc-major storage) -- identical to the
// reference Python evaluator's (arcs x slots) array.
package eval

import (
	"fmt"
	"math"
	"sort"

	"tasr/internal/ecmp"
	"tasr/internal/graph"
	"tasr/internal/model"
)

// ScaledEps absorbs floating-point noise when a value sits a hair below a
// decimal boundary but is mathematically exactly on it (reference objective.py).
const ScaledEps = 1e-6
const Scale = 1e6

// RankInt quantizes one saturation to an exact integer floor(x*1e6 + eps).
func RankInt(x float64) int64 {
	return int64(math.Floor(x*Scale + ScaledEps))
}

// RankMatrix quantizes a whole saturation matrix.
func RankMatrix(sat []float64) []int64 {
	out := make([]int64, len(sat))
	for i, x := range sat {
		out[i] = RankInt(x)
	}
	return out
}

// Evaluator computes saturation matrices for Solutions.
type Evaluator struct {
	inst  *model.Instance
	g     *graph.Graph
	cache *ecmp.Cache
	m     int
	T     int
}

func NewEvaluator(inst *model.Instance, g *graph.Graph, cache *ecmp.Cache) *Evaluator {
	return &Evaluator{inst: inst, g: g, cache: cache, m: g.M, T: inst.NSlots}
}

// Segments returns the consecutive (u, v) node pairs of a segment path, empty
// segments skipped (equal consecutive points collapse away).
func (e *Evaluator) Segments(d *model.Demand, waypoints []int) [][2]int {
	pts := make([]int, 0, len(waypoints)+2)
	pts = append(pts, d.Source)
	pts = append(pts, waypoints...)
	pts = append(pts, d.Target)
	segs := [][2]int{}
	for i := 0; i+1 < len(pts); i++ {
		if pts[i] != pts[i+1] {
			segs = append(segs, [2]int{pts[i], pts[i+1]})
		}
	}
	return segs
}

// Saturations returns the saturation matrix (arcs x slots) for a solution,
// mirroring the reference evaluator's dense superposition exactly (same loop
// order and arithmetic, so results are bit-identical).
func (e *Evaluator) Saturations(sol *model.Solution) ([]float64, error) {
	m, T := e.m, e.T
	flows := make([]float64, m*T)
	for d, dem := range e.inst.Demands {
		for t := 0; t < T; t++ {
			vol := dem.Volume[t]
			if vol == 0.0 {
				continue
			}
			acc := make([]float64, m)
			for _, seg := range e.Segments(&dem, sol.Waypoints[d][t]) {
				atom := e.cache.Atom(seg[0], seg[1], t)
				if atom == nil {
					return nil, fmt.Errorf("demand %d segment %d->%d disconnected at slot %d",
						d, seg[0], seg[1], t)
				}
				for i := range acc {
					acc[i] += atom[i]
				}
			}
			base := t
			for i, a := range acc {
				flows[i*T+base] += vol * a
			}
		}
	}
	sat := make([]float64, m*T)
	for a := 0; a < m; a++ {
		cap := e.g.Cap[a]
		for t := 0; t < T; t++ {
			sat[a*T+t] = flows[a*T+t] / cap
		}
	}
	return sat, nil
}

// SortedDesc returns a fresh copy of the saturation matrix flattened and sorted
// in decreasing order.
func SortedDesc(sat []float64) []float64 {
	v := append([]float64(nil), sat...)
	sort.Slice(v, func(i, j int) bool { return v[i] > v[j] })
	return v
}

// LexCompare compares two sorted-descending saturation vectors under the
// official rule (truncate to 6 decimals, then lexicographic; smaller is
// better).  Returns -1 if a wins, +1 if b wins, 0 on a tie.
func LexCompare(aDesc, bDesc []float64) int {
	ia := RankMatrix(aDesc)
	ib := RankMatrix(bDesc)
	for i := range ia {
		if ia[i] < ib[i] {
			return -1
		}
		if ia[i] > ib[i] {
			return 1
		}
	}
	return 0
}

// FirstDivergence reports the first position (1-based layer) at which two
// saturated-desc vectors differ after truncation, or 0 when they tie fully.
// Also returns the two truncated values at that layer.
func FirstDivergence(aDesc, bDesc []float64) (layer int, aQ, bQ int64) {
	n := len(aDesc)
	if len(bDesc) < n {
		n = len(bDesc)
	}
	for i := 0; i < n; i++ {
		qa, qb := RankInt(aDesc[i]), RankInt(bDesc[i])
		if qa != qb {
			return i + 1, qa, qb
		}
	}
	return 0, 0, 0
}
