// Package mip implements the M2 decompose-round: it extracts the demands that
// load a hot arc, generates alternative waypoint routings that relieve it, and
// solves a small Gurobi MIP that picks one alternative per demand to minimise
// the global maximum saturation (first bit), subject to the Hamming budget.
//
// The MIP works over one time slot t.  Arc loads of every (demand, candidate)
// at that slot are precomputed exactly with the ECMP engine (snap.UnitRoute),
// so the model is a pure 0-1 selection problem with no embedded shortest paths.
package mip

import (
	"sort"

	"tasr/internal/graph"
	"tasr/internal/model"
	"tasr/internal/snap"
)

// Candidate is one waypoint choice for a (d, t) pair.  Load[a] is the load this
// candidate puts on arc a at the focus slot (volume * ECMP fraction), length g.M.
type Candidate struct {
	Wps  []int
	Load []float64
}

// Pair groups the alternatives of one (d, t).
type Pair struct {
	D    int
	T    int
	Cand []Candidate // index 0 is always the current routing
}

// Pool is the candidate pool for one focus slot.
type Pool struct {
	inst  *model.Instance
	g     *graph.Graph
	snap  *snap.Snap
	t     int
	m     int
	Pairs []Pair
}

// Generator builds a pool for the given focus slot and hot arc position.
type Generator struct {
	Inst             *model.Instance
	G                *graph.Graph
	Snap             *snap.Snap
	MaxCandPerDemand int     // cap of alternatives kept per demand (excluding current)
	FracEps          float64 // treat frac below this as zero on the hot arc
}

// Build returns the pool of demands whose current routing at slot t loads arcA,
// together with single-waypoint alternatives that strictly reduce that load.
func (gen *Generator) Build(t, arcA int) (*Pool, error) {
	if gen.MaxCandPerDemand <= 0 {
		gen.MaxCandPerDemand = 8
	}
	if gen.FracEps <= 0 {
		gen.FracEps = 1e-9
	}
	inst, g, sn := gen.Inst, gen.G, gen.Snap
	m, T := g.M, inst.NSlots
	p := &Pool{inst: inst, g: g, snap: sn, t: t, m: m}

	n := inst.NNodes()
	for d := 0; d < inst.NDemands(); d++ {
		dem := &inst.Demands[d]
		vol := dem.Volume[t]
		if vol == 0.0 {
			continue
		}
		cur := append([]int(nil), sn.GetWaypoints(d, t)...)
		curUnit, err := sn.UnitRoute(d, t, cur)
		if err != nil {
			continue // current routing somehow disconnected: skip
		}
		curLoadA := vol * curUnit[arcA]
		if curLoadA <= gen.FracEps {
			continue // does not load the hot arc at this slot
		}

		type alt struct {
			wps  []int
			unit []float64
			load []float64
			rel  float64 // relief on hot arc
		}
		var alts []alt
		seen := map[string]bool{}
		key := func(w []int) string {
			b := make([]byte, 0, len(w)*4)
			for _, x := range w {
				b = append(b, byte(x>>8), byte(x))
			}
			return string(b)
		}
		for w := 0; w < n; w++ {
			if w == dem.Source || w == dem.Target {
				continue
			}
			wps := []int{w}
			if seen[key(wps)] {
				continue
			}
			unit, err := sn.UnitRoute(d, t, wps)
			if err != nil {
				continue
			}
			if vol*unit[arcA] < curLoadA-gen.FracEps {
				seen[key(wps)] = true
				alts = append(alts, alt{wps: wps, unit: unit, load: scaleUnit(unit, vol), rel: curLoadA - vol*unit[arcA]})
			}
		}
		if len(alts) == 0 {
			continue // no single waypoint relieves this demand on arcA
		}
		sort.Slice(alts, func(i, j int) bool { return alts[i].rel > alts[j].rel })
		if len(alts) > gen.MaxCandPerDemand {
			alts = alts[:gen.MaxCandPerDemand]
		}
		pair := Pair{D: d, T: t}
		pair.Cand = append(pair.Cand, Candidate{Wps: cur, Load: scaleUnit(curUnit, vol)})
		for _, a := range alts {
			pair.Cand = append(pair.Cand, Candidate{Wps: a.wps, Load: a.load})
		}
		p.Pairs = append(p.Pairs, pair)
	}
	_ = T
	return p, nil
}

func scaleUnit(unit []float64, vol float64) []float64 {
	out := make([]float64, len(unit))
	for i := range unit {
		out[i] = vol * unit[i]
	}
	return out
}
