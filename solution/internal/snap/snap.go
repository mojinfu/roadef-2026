// Package snap implements RoutingSnapshot: the solver's mutable working state.
//
// A Snapshot holds the current waypoints of every (demand, slot) together with
// the induced arc load (arcs x slots).  Patch() re-routes only the demands
// whose waypoints changed and updates the load incrementally, so accept /
// rollback of a candidate routing never requires a full re-evaluation.
//
// The Hamming budget helpers reproduce, exactly, the official checker's cost
// (checker/src/helpers.h): the distance between two consecutive SR paths of one
// demand is
//
//	0                      if both are empty (no waypoints),
//	len(wps)+2             if exactly one side is empty (the other SR path has
//	                       that many nodes: s + waypoints + t),
//	|maskA △ maskB|        otherwise, where mask = set of consecutive node
//	                       pairs (a,b) along s + waypoints + t.
//
// Saturation matrix layout is the same as internal/eval: flat [arc*T + slot].
package snap

import (
	"fmt"
	"sort"

	"tasr/internal/ecmp"
	"tasr/internal/graph"
	"tasr/internal/model"
)

// Key identifies one (time slot, arc).
type Key struct {
	T int
	A int
}

type Snap struct {
	inst  *model.Instance
	g     *graph.Graph
	cache *ecmp.Cache
	m     int
	T     int
	nD    int

	wp   [][][]int // waypoints per (d, t), internal positions
	load []float64 // arc-major: [a*T + t]
}

// New builds a Snapshot from an initial solution, fully re-routing every
// (d, t) with positive volume.
func New(inst *model.Instance, g *graph.Graph, cache *ecmp.Cache, sol *model.Solution) (*Snap, error) {
	m, T, nD := g.M, inst.NSlots, inst.NDemands()
	s := &Snap{
		inst: inst, g: g, cache: cache,
		m: m, T: T, nD: nD,
		wp:   make([][][]int, nD),
		load: make([]float64, m*T),
	}
	for d := 0; d < nD; d++ {
		s.wp[d] = make([][]int, T)
		for t := 0; t < T; t++ {
			s.wp[d][t] = append([]int(nil), sol.Waypoints[d][t]...)
		}
	}
	if err := s.rerouteAll(); err != nil {
		return nil, err
	}
	return s, nil
}

// NewEmptySnapshot builds a Snapshot for the empty solution (direct SP for all).
func NewEmpty(inst *model.Instance, g *graph.Graph, cache *ecmp.Cache) (*Snap, error) {
	return New(inst, g, cache, model.EmptySolution(inst.NDemands(), inst.NSlots))
}

// UnitRoute returns the dense arc-load vector (length m) of one unit of demand
// d at slot t routed through wps (sum of per-segment ECMP atoms).
func (s *Snap) UnitRoute(d, t int, wps []int) ([]float64, error) {
	dem := &s.inst.Demands[d]
	acc := make([]float64, s.m)
	for _, seg := range s.segments(dem, wps) {
		atom := s.cache.Atom(seg[0], seg[1], t)
		if atom == nil {
			return nil, fmt.Errorf("demand %d segment %d->%d disconnected at slot %d",
				d, seg[0], seg[1], t)
		}
		for i := range acc {
			acc[i] += atom[i]
		}
	}
	return acc, nil
}

func (s *Snap) segments(dem *model.Demand, wps []int) [][2]int {
	pts := make([]int, 0, len(wps)+2)
	pts = append(pts, dem.Source)
	pts = append(pts, wps...)
	pts = append(pts, dem.Target)
	segs := [][2]int{}
	for i := 0; i+1 < len(pts); i++ {
		if pts[i] != pts[i+1] {
			segs = append(segs, [2]int{pts[i], pts[i+1]})
		}
	}
	return segs
}

func (s *Snap) rerouteAll() error {
	for d := 0; d < s.nD; d++ {
		dem := &s.inst.Demands[d]
		for t := 0; t < s.T; t++ {
			vol := dem.Volume[t]
			if vol == 0.0 {
				continue
			}
			u, err := s.UnitRoute(d, t, s.wp[d][t])
			if err != nil {
				return err
			}
			s.addUnit(d, t, u, vol)
		}
	}
	return nil
}

// Rebuild recomputes all loads from scratch from the current waypoints.  Use
// it to re-sync the load vector after a long sequence of incremental patches
// (each subtract/add can drift a few ulps).
func (s *Snap) Rebuild() error {
	for i := range s.load {
		s.load[i] = 0.0
	}
	return s.rerouteAll()
}

func (s *Snap) addUnit(d, t int, u []float64, vol float64) {
	for a := range u {
		if u[a] != 0.0 {
			s.load[a*s.T+t] += vol * u[a]
		}
	}
}

// Waypoints returns a deep copy of the current waypoints as a rectangular
// model.Solution (ready for the srpaths writer).
func (s *Snap) Solution() *model.Solution {
	sol := model.EmptySolution(s.nD, s.T)
	for d := range s.wp {
		for t := range s.wp[d] {
			if len(s.wp[d][t]) > 0 {
				sol.Waypoints[d][t] = append([]int(nil), s.wp[d][t]...)
			}
		}
	}
	return sol
}

// GetWaypoints returns the current waypoint list of (d, t) (do not mutate).
func (s *Snap) GetWaypoints(d, t int) []int { return s.wp[d][t] }

// SetWaypoints updates one (d, t) pair, re-routing that demand incrementally.
// It returns the previous waypoints (for rollback).
func (s *Snap) SetWaypoints(d, t int, wps []int) ([]int, error) {
	dem := &s.inst.Demands[d]
	vol := dem.Volume[t]
	old := s.wp[d][t]
	if vol == 0.0 {
		s.wp[d][t] = append([]int(nil), wps...)
		return old, nil
	}
	if equalWaypoints(old, wps) {
		return old, nil
	}
	// Remove old contribution, add new.
	uOld, err := s.UnitRoute(d, t, old)
	if err != nil {
		return nil, err
	}
	for a := range uOld {
		if uOld[a] != 0.0 {
			s.load[a*s.T+t] -= vol * uOld[a]
		}
	}
	uNew, err := s.UnitRoute(d, t, wps)
	if err != nil {
		// Roll the subtraction back: keep the previous state consistent.
		uOld, err2 := s.UnitRoute(d, t, old)
		if err2 != nil {
			return nil, err
		}
		for a := range uOld {
			if uOld[a] != 0.0 {
				s.load[a*s.T+t] += vol * uOld[a]
			}
		}
		return nil, err
	}
	for a := range uNew {
		if uNew[a] != 0.0 {
			s.load[a*s.T+t] += vol * uNew[a]
		}
	}
	s.wp[d][t] = append([]int(nil), wps...)
	return old, nil
}

// Patch applies a batch of waypoint changes for one slot t.  changes maps a
// demand id to its new waypoint list.  Only the changed (d, t) are re-routed.
func (s *Snap) Patch(t int, changes map[int][]int) error {
	for d, wps := range changes {
		if _, err := s.SetWaypoints(d, t, wps); err != nil {
			return err
		}
	}
	return nil
}

// Saturations returns load / capacity as a fresh arc-major matrix.
func (s *Snap) Saturations() []float64 {
	sat := make([]float64, s.m*s.T)
	for a := 0; a < s.m; a++ {
		cap := s.g.Cap[a]
		for t := 0; t < s.T; t++ {
			sat[a*s.T+t] = s.load[a*s.T+t] / cap
		}
	}
	return sat
}

// Load returns the raw load matrix (arc-major).  Do not mutate.
func (s *Snap) Load() []float64 { return s.load }

// RankKeys returns (t, arc) keys sorted by decreasing saturation.  skip keys
// are excluded.  Ties are broken by (t, arc) for determinism.
func (s *Snap) RankKeys(skip map[Key]bool) []Key {
	type ka struct {
		k Key
		v float64
	}
	sat := s.Saturations()
	list := make([]ka, 0, s.m*s.T)
	for a := 0; a < s.m; a++ {
		for t := 0; t < s.T; t++ {
			k := Key{T: t, A: a}
			if skip[k] {
				continue
			}
			list = append(list, ka{k, sat[a*s.T+t]})
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].v != list[j].v {
			return list[i].v > list[j].v
		}
		if list[i].k.T != list[j].k.T {
			return list[i].k.T < list[j].k.T
		}
		return list[i].k.A < list[j].k.A
	})
	out := make([]Key, len(list))
	for i := range list {
		out[i] = list[i].k
	}
	return out
}

// CostAt returns the total inter-slot Hamming cost of the t-1 -> t transition
// (summed over all demands), i.e. the checker's cost at time step t.
func (s *Snap) CostAt(t int) int {
	return WaypointsCostAt(s.inst, s.wp, t)
}

// TotalCost returns the checker's total_cost: sum over t=1..T-1 of CostAt(t).
func (s *Snap) TotalCost() int {
	return WaypointsTotalCost(s.inst, s.wp)
}

// BudgetOK reports whether every transition respects the scenario budget.
func (s *Snap) BudgetOK() bool {
	return BudgetOK(s.inst, s.wp)
}

func equalWaypoints(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
