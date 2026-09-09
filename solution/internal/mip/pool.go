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
	"fmt"
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
	return gen.BuildKeys(t, []int{arcA})
}

// BuildKeys returns the pool of demands whose current routing at slot t loads
// at least one of the hot arcs, with single-waypoint alternatives that strictly
// reduce the load of at least one hot arc.  Candidates are deduplicated across
// hot arcs and ranked by the largest relief they give to any one of them.
func (gen *Generator) BuildKeys(t int, hotArcs []int) (*Pool, error) {
	if gen.MaxCandPerDemand <= 0 {
		gen.MaxCandPerDemand = 8
	}
	if gen.FracEps <= 0 {
		gen.FracEps = 1e-9
	}
	if len(hotArcs) == 0 {
		return nil, fmt.Errorf("mip: BuildKeys with empty hot-arc set")
	}
	inst, g, sn := gen.Inst, gen.G, gen.Snap
	m := g.M
	p := &Pool{inst: inst, g: g, snap: sn, t: t, m: m}

	keyOf := func(w []int) string {
		b := make([]byte, 0, len(w)*4)
		for _, x := range w {
			b = append(b, byte(x>>8), byte(x))
		}
		return string(b)
	}

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
		curLoad := scaleUnit(curUnit, vol)

		// Which hot arcs does this demand currently load?
		loadsHot := false
		for _, ha := range hotArcs {
			if curLoad[ha] > gen.FracEps {
				loadsHot = true
				break
			}
		}
		if !loadsHot {
			continue
		}

		reliefOn := func(unit []float64, ha int) float64 {
			return curLoad[ha] - vol*unit[ha]
		}
		bestRel := func(unit []float64) float64 {
			best := -1.0
			for _, ha := range hotArcs {
				if r := reliefOn(unit, ha); r > best {
					best = r
				}
			}
			return best
		}

		type alt struct {
			wps  []int
			unit []float64
			rel  float64
		}
		var alts []alt
		seen := map[string]bool{}
		var nodes []int
		for w := 0; w < n; w++ {
			if w == dem.Source || w == dem.Target {
				continue
			}
			nodes = append(nodes, w)
		}
		// Single-waypoint reroutes.
		for _, w := range nodes {
			wps := []int{w}
			k := keyOf(wps)
			seen[k] = true
			unit, err := sn.UnitRoute(d, t, wps)
			if err != nil {
				continue
			}
			if rel := bestRel(unit); rel > gen.FracEps {
				alts = append(alts, alt{wps: wps, unit: unit, rel: rel})
			}
		}
		// Two-waypoint reroutes (unordered pairs).  A single waypoint cannot
		// avoid an arc that lies on every shortest path into an intermediate
		// node; splitting the path twice can detour around such a cut.
		for i := 0; i < len(nodes); i++ {
			for j := i + 1; j < len(nodes); j++ {
				wps := []int{nodes[i], nodes[j]}
				k := keyOf(wps)
				if seen[k] {
					continue
				}
				seen[k] = true
				unit, err := sn.UnitRoute(d, t, wps)
				if err != nil {
					continue
				}
				if rel := bestRel(unit); rel > gen.FracEps {
					alts = append(alts, alt{wps: wps, unit: unit, rel: rel})
				}
			}
		}
		if len(alts) == 0 {
			continue // no 1-wp or 2-wp reroute relieves any hot arc for this demand
		}
		// Rank by relief, keep the top MaxCandPerDemand.
		sort.Slice(alts, func(i, j int) bool { return alts[i].rel > alts[j].rel })
		if len(alts) > gen.MaxCandPerDemand {
			alts = alts[:gen.MaxCandPerDemand]
		}
		pair := Pair{D: d, T: t}
		pair.Cand = append(pair.Cand, Candidate{Wps: cur, Load: curLoad})
		for _, a := range alts {
			pair.Cand = append(pair.Cand, Candidate{Wps: a.wps, Load: scaleUnit(a.unit, vol)})
		}
		p.Pairs = append(p.Pairs, pair)
	}
	return p, nil
}

func scaleUnit(unit []float64, vol float64) []float64 {
	out := make([]float64, len(unit))
	for i := range unit {
		out[i] = vol * unit[i]
	}
	return out
}
