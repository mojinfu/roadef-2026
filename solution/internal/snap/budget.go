package snap

import "tasr/internal/model"

// Pure Hamming-budget helpers over a waypoint layout wp[d][t] (internal node
// positions).  They reproduce the official checker's per-demand distance
// between consecutive SR paths (checker/src/helpers.h, details::distEx):
//
//   - empty (no waypoint list) on one side and a k-waypoint path on the other:
//     distance = k+2, the node count of s + waypoints + t of the non-empty
//     path (the checker's SrPathBit.segmentNum());
//   - both empty: 0;
//   - both non-empty: Hamming distance between the two sets of consecutive
//     directed node pairs (a, b) along s + waypoints + t.

// PathDist computes the checker distance between two consecutive SR paths of
// demand d (waypoints wpA at t-1, wpB at t).
func PathDist(inst *model.Instance, d int, wpA, wpB []int) int {
	la, lb := len(wpA), len(wpB)
	if la == 0 && lb == 0 {
		return 0
	}
	if la == 0 {
		return lb + 2 // node count s + waypoints + t
	}
	if lb == 0 {
		return la + 2
	}
	dem := &inst.Demands[d]
	setA := pairMask(inst, dem.Source, dem.Target, wpA)
	setB := pairMask(inst, dem.Source, dem.Target, wpB)
	diff := 0
	for p := range setA {
		if _, ok := setB[p]; !ok {
			diff++
		}
	}
	for p := range setB {
		if _, ok := setA[p]; !ok {
			diff++
		}
	}
	return diff
}

func pairMask(inst *model.Instance, s, t int, wps []int) map[int]struct{} {
	n := inst.NNodes()
	mask := make(map[int]struct{}, len(wps)+1)
	prev := s
	for _, w := range wps {
		mask[prev*n+w] = struct{}{}
		prev = w
	}
	mask[prev*n+t] = struct{}{}
	return mask
}

// WaypointsCostAt sums PathDist over all demands for the t-1 -> t transition.
func WaypointsCostAt(inst *model.Instance, wp [][][]int, t int) int {
	cost := 0
	for d := 0; d < len(wp); d++ {
		cost += PathDist(inst, d, wp[d][t-1], wp[d][t])
	}
	return cost
}

// WaypointsTotalCost sums WaypointsCostAt over t=1..T-1 (the checker total_cost).
func WaypointsTotalCost(inst *model.Instance, wp [][][]int) int {
	total := 0
	for t := 1; t < inst.NSlots; t++ {
		total += WaypointsCostAt(inst, wp, t)
	}
	return total
}

// SolutionCostAt is WaypointsCostAt over a rectangular Solution.
func SolutionCostAt(inst *model.Instance, sol *model.Solution, t int) int {
	return WaypointsCostAt(inst, sol.Waypoints, t)
}

// SolutionTotalCost is WaypointsTotalCost over a rectangular Solution.
func SolutionTotalCost(inst *model.Instance, sol *model.Solution) int {
	return WaypointsTotalCost(inst, sol.Waypoints)
}

// BudgetOK reports whether every transition t=1..T-1 satisfies the scenario
// budget, i.e. WaypointsCostAt(t) <= inst.Scenario.Budget[t] (the checker's
// validity rule: an srpaths solution that exceeds any budget is invalid).
func BudgetOK(inst *model.Instance, wp [][][]int) bool {
	for t := 1; t < inst.NSlots; t++ {
		if t-1 >= len(inst.Scenario.Budget) {
			continue // no budget declared for this transition
		}
		if WaypointsCostAt(inst, wp, t) > inst.Scenario.Budget[t] {
			return false
		}
	}
	return true
}
