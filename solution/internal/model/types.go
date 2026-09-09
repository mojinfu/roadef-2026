// Package model holds the pure-data representation of a T-ASR instance and of
// a routing scheme (solution).  It mirrors the reference Python model in
// solution/_pyref/tasr/model: node references are contiguous positions
// 0..n-1 assigned in the order the network file lists them; the mapping back to
// the file's node ids lives in Instance.NodeIDs and is applied at the I/O
// boundary only.
package model

import "fmt"

// Arc is one directed link.  ID always equals its index in Instance.Arcs.
type Arc struct {
	ID       int
	From     int // node position
	To       int // node position
	Metric   float64
	Capacity float64
}

// Demand is a (source, target) couple with one traffic volume per slot.
type Demand struct {
	Source int
	Target int
	Volume []float64 // length == n_slots
}

// Scenario carries max_segments plus, per time slot, the reconfiguration
// budget (budget[0] == 0, unused) and the set of down arc ids (interventions).
type Scenario struct {
	MaxSegments int
	Budget      []int    // length n_slots; Budget[t] applies to the t-1 -> t transition
	Blocked     [][]bool // Blocked[t][arcID] true => arc is down at slot t
}

// Instance is a complete challenge instance.
type Instance struct {
	Name      string
	NodeNames []string // position -> display name
	NodeIDs   []int    // position -> network-file node id
	Arcs      []Arc    // arc id == index
	NSlots    int
	Demands   []Demand // demand id == index in the JSON array
	Scenario  Scenario
}

func (inst *Instance) NNodes() int   { return len(inst.NodeNames) }
func (inst *Instance) NArcs() int    { return len(inst.Arcs) }
func (inst *Instance) NDemands() int { return len(inst.Demands) }

// NodeIndex maps a network-file node id back to its internal position.
func (inst *Instance) NodeIndex() map[int]int {
	ix := make(map[int]int, len(inst.NodeIDs))
	for i, id := range inst.NodeIDs {
		ix[id] = i
	}
	return ix
}

// CheckInterventions ensures every intervention arc id is in range.
func (inst *Instance) CheckInterventions() error {
	m := inst.NArcs()
	for t, bl := range inst.Scenario.Blocked {
		if len(bl) != m {
			return fmt.Errorf("slot %d blocked mask length %d != n_arcs %d", t, len(bl), m)
		}
	}
	return nil
}

// Solution is the segment path chosen for every (demand, slot).
//
// Waypoints[d][t] lists waypoint node positions excluding source and target;
// an empty slice means plain shortest-path routing.  The array is always
// rectangular (every demand has exactly NSlots entries) so budget and delta
// evaluation stay uniform.
type Solution struct {
	NDemands  int
	NSlots    int
	Waypoints [][][]int
}

// EmptySolution returns the trivial solution (no waypoint anywhere).
func EmptySolution(nDemands, nSlots int) *Solution {
	wps := make([][][]int, nDemands)
	for d := range wps {
		wps[d] = make([][]int, nSlots)
	}
	return &Solution{NDemands: nDemands, NSlots: nSlots, Waypoints: wps}
}

// Copy returns a deep copy.
func (s *Solution) Copy() *Solution {
	c := EmptySolution(s.NDemands, s.NSlots)
	for d := range s.Waypoints {
		for t := range s.Waypoints[d] {
			if len(s.Waypoints[d][t]) > 0 {
				c.Waypoints[d][t] = append([]int(nil), s.Waypoints[d][t]...)
			}
		}
	}
	return c
}

// Set replaces the waypoint list of one (d, t).
func (s *Solution) Set(d, t int, wps []int) {
	s.Waypoints[d][t] = append([]int(nil), wps...)
}
