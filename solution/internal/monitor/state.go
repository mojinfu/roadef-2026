// Live-view state model and snapshot builder.
//
// BuildState projects a RoutingSnapshot (plus the solve loop's done/fail book-
// keeping) onto the compact dashboard JSON: the global top-N hottest cells, the
// per-slot top-K bars the page draws, and the "currently solving" hot cells,
// each annotated with frozen / confidence / hot flags.  The solve loop calls it
// once per round and hands the result to Server.Publish.
package monitor

import (
	"fmt"
	"math"
	"sort"

	"tasr/internal/eval"
	"tasr/internal/graph"
	"tasr/internal/model"
	"tasr/internal/snap"
)

// Cell is one (slot, arc) cell as drawn on the page.  Conf is the freeze
// confidence: 1 for a frozen (done) cell, fail/failLimit for an attempted cell
// that has not frozen yet, and -1 when the arc was never tried (no cap drawn).
type Cell struct {
	T         int     `json:"t"`
	A         int     `json:"a"`
	Sat       float64 `json:"sat"`
	Rank      int64   `json:"rank"`
	From      int     `json:"from"`
	To        int     `json:"to"`
	Tag       string  `json:"tag"` // "nodeFrom→nodeTo" in file ids
	Cap       float64 `json:"cap"`
	Load      float64 `json:"load"`
	Frozen    bool    `json:"frozen"`
	Conf      float64 `json:"conf"`
	Hot       bool    `json:"hot"` // member of the current decompose focus
	Attempted bool    `json:"attempted"`
}

// Slot is one time slot column of the bar chart.
type Slot struct {
	T     int    `json:"t"`
	Focus bool   `json:"focus"` // focus slot of the current round
	Bars  []Cell `json:"bars"`
}

// State is one full dashboard snapshot.
type State struct {
	Instance  string `json:"instance"`
	Status    string `json:"status"` // "running" | "done" | "idle"
	Message   string `json:"message,omitempty"`
	Round     int    `json:"round"`
	MaxRounds int    `json:"max_rounds"`
	ElapsedMS int64  `json:"elapsed_ms"`

	Accepted  int  `json:"accepted"`
	Rejected  int  `json:"rejected"`
	TotalCost int  `json:"total_cost"`
	BudgetOK  bool `json:"budget_ok"`

	FirstBit  float64 `json:"first_bit"`  // raw current max saturation
	FirstRank int64   `json:"first_rank"` // trunc6 integer rank (= mlu6)
	StartRank int64   `json:"start_rank"` // rank of the seed (empty) snapshot
	NSlots    int     `json:"n_slots"`

	Top       []Cell `json:"top"`        // global top-N hottest cells
	Hots      []Cell `json:"hots"`       // cells the MIP is working on right now
	FocusSlot int    `json:"focus_slot"` // -1 when nothing is being solved
	Slots     []Slot `json:"slots"`      // one column per time slot, t ascending
	BarTop    int    `json:"bar_top"`

	FailLimit   int    `json:"fail_limit"`
	Note        string `json:"note,omitempty"`
	ServerNowMS int64  `json:"server_now_ms"`
}

// BuildOpts carries everything the solve loop knows that is not derivable from
// the snapshot itself (round counters, the current focus, freeze memory).
type BuildOpts struct {
	Instance  string
	Status    string
	Message   string
	Round     int
	MaxRounds int
	ElapsedMS int64
	Accepted  int
	Rejected  int
	TotalCost int
	BudgetOK  bool
	StartRank int64
	FailLimit int
	Hots      []snap.Key
	Done      map[snap.Key]bool
	Fail      map[snap.Key]int
	BarTop    int // bars per slot (page default 10)
	TopN      int // global top list length (page default 10)
	Note      string
}

type cellv struct {
	k snap.Key
	v float64
}

// BuildState computes one immutable dashboard snapshot.  It is called from the
// solve goroutine only; Server.Publish swaps the pointer atomically, so no
// further copy is needed.
func BuildState(inst *model.Instance, g *graph.Graph, sn *snap.Snap, o BuildOpts) *State {
	m, T := g.M, inst.NSlots
	if o.BarTop <= 0 {
		o.BarTop = 10
	}
	if o.TopN <= 0 {
		o.TopN = 10
	}
	if o.FailLimit <= 0 {
		o.FailLimit = 2
	}
	sat := sn.Saturations()
	load := sn.Load()

	hotSet := map[snap.Key]bool{}
	for _, k := range o.Hots {
		hotSet[k] = true
	}

	nodeID := func(pos int) int {
		if pos >= 0 && pos < len(inst.NodeIDs) {
			return inst.NodeIDs[pos]
		}
		return pos
	}
	tag := func(a int) string {
		return fmt.Sprintf("%d→%d", nodeID(g.From[a]), nodeID(g.To[a]))
	}
	// Freeze memory of one cell.
	meta := func(k snap.Key, hot bool) (frozen bool, conf float64, attempted bool) {
		if o.Done[k] {
			return true, 1, true
		}
		if f := o.Fail[k]; f > 0 {
			return false, math.Min(1, float64(f)/float64(o.FailLimit)), true
		}
		return false, -1, hot
	}
	mkCell := func(k snap.Key, v float64) Cell {
		frozen, conf, attempted := meta(k, hotSet[k])
		return Cell{
			T: k.T, A: k.A, Sat: v, Rank: eval.RankInt(v),
			From: g.From[k.A], To: g.To[k.A], Tag: tag(k.A),
			Cap: g.Cap[k.A], Load: load[k.A*T+k.T],
			Frozen: frozen, Conf: conf, Hot: hotSet[k], Attempted: attempted,
		}
	}

	// One scan, sorted once: the global top list is a prefix and each slot's
	// bars are its slice filtered in order (already sat-descending).
	all := make([]cellv, 0, m*T)
	firstBit := 0.0
	for a := 0; a < m; a++ {
		base := a * T
		for t := 0; t < T; t++ {
			v := sat[base+t]
			if v <= 0 {
				continue
			}
			if v > firstBit {
				firstBit = v
			}
			all = append(all, cellv{snap.Key{T: t, A: a}, v})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].v != all[j].v {
			return all[i].v > all[j].v
		}
		if all[i].k.T != all[j].k.T {
			return all[i].k.T < all[j].k.T
		}
		return all[i].k.A < all[j].k.A
	})

	focus := -1
	if len(o.Hots) > 0 {
		focus = o.Hots[0].T
	}
	slots := make([]Slot, T)
	for i := range slots {
		slots[i].T = i
		slots[i].Focus = i == focus
	}
	for _, cv := range all {
		tt := cv.k.T
		if len(slots[tt].Bars) >= o.BarTop {
			continue
		}
		slots[tt].Bars = append(slots[tt].Bars, mkCell(cv.k, cv.v))
	}

	top := make([]Cell, 0, o.TopN)
	for i, cv := range all {
		if i >= o.TopN {
			break
		}
		top = append(top, mkCell(cv.k, cv.v))
	}

	hots := make([]Cell, 0, len(o.Hots))
	for _, k := range o.Hots {
		hots = append(hots, mkCell(k, sat[k.A*T+k.T]))
	}

	return &State{
		Instance: inst.Name, Status: o.Status, Message: o.Message,
		Round: o.Round, MaxRounds: o.MaxRounds, ElapsedMS: o.ElapsedMS,
		Accepted: o.Accepted, Rejected: o.Rejected,
		TotalCost: o.TotalCost, BudgetOK: o.BudgetOK,
		FirstBit: firstBit, FirstRank: eval.RankInt(firstBit), StartRank: o.StartRank,
		NSlots: T,
		Top:    top, Hots: hots, FocusSlot: focus, Slots: slots, BarTop: o.BarTop,
		FailLimit: o.FailLimit, Note: o.Note,
	}
}
