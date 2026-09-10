// Command hotarcs reports the hottest arcs of a solution: it re-evaluates the
// ECMP saturations with the same evaluator the solver uses and prints the
// arc/slot cells with the largest load/capacity ratio.
//
// This is a read-only reporting tool.  It is meant to answer "what is still
// binding after the search stopped", which is what decides the next move.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"

	"tasr/internal/ecmp"
	"tasr/internal/eval"
	"tasr/internal/graph"
	"tasr/internal/io"
)

func main() {
	prefix := flag.String("prefix", "", "instance prefix, e.g. ../setB/setB-01")
	solPath := flag.String("sol", "", "solution JSON to evaluate")
	top := flag.Int("top", 10, "how many arcs to print")
	perSlot := flag.Bool("per-slot", false, "rank cells (arc,slot) instead of arcs (max over slots)")
	flag.Parse()

	if *prefix == "" || *solPath == "" {
		fmt.Fprintln(os.Stderr, "usage: hotarcs -prefix <inst> -sol <solution.json>")
		os.Exit(2)
	}

	inst, err := io.LoadInstance(*prefix)
	if err != nil {
		fmt.Fprintln(os.Stderr, "hotarcs: load instance:", err)
		os.Exit(1)
	}
	g := graph.New(inst)
	sol, err := io.ReadSolution(*solPath, inst)
	if err != nil {
		fmt.Fprintln(os.Stderr, "hotarcs: load solution:", err)
		os.Exit(1)
	}

	cache := ecmp.NewCache(g, inst.Scenario.Blocked, 0)
	ev := eval.NewEvaluator(inst, g, cache)
	sat, err := ev.Saturations(sol)
	if err != nil {
		fmt.Fprintln(os.Stderr, "hotarcs: evaluate:", err)
		os.Exit(1)
	}

	m, T := g.M, inst.NSlots
	desc := eval.SortedDesc(sat)
	fmt.Printf("instance=%s  arcs=%d  slots=%d  cells=%d\n", inst.Name, m, T, len(sat))
	fmt.Printf("MLU (1st component, trunc6) = %.6f\n", desc[0])
	for i := 0; i < 5; i++ {
		fmt.Printf("  top%-2d %.6f\n", i+1, desc[i])
	}
	fmt.Println()

	if *perSlot {
		type cell struct {
			a, t int
			v    float64
		}
		cells := make([]cell, 0, m*T)
		for a := 0; a < m; a++ {
			for t := 0; t < T; t++ {
				cells = append(cells, cell{a, t, sat[a*T+t]})
			}
		}
		sort.Slice(cells, func(i, j int) bool { return cells[i].v > cells[j].v })
		fmt.Printf("rank  arc    slot  saturation   from -> to\n")
		for i := 0; i < *top && i < len(cells); i++ {
			c := cells[i]
			fmt.Printf("%4d  %5d  %4d  %10.6f   %s -> %s\n", i+1, c.a, c.t, c.v,
				inst.NodeNames[g.From[c.a]], inst.NodeNames[g.To[c.a]])
		}
		return
	}

	// Arc level: an arc's heat is its worst slot, but keep the slot so the
	// reader can tell a persistently hot arc from a one-slot spike.
	type arcHeat struct {
		a, t int
		v    float64
		sum  float64
	}
	heats := make([]arcHeat, m)
	for a := 0; a < m; a++ {
		best, bestT, sum := sat[a*T], 0, 0.0
		for t := 0; t < T; t++ {
			v := sat[a*T+t]
			sum += v
			if v > best {
				best, bestT = v, t
			}
		}
		heats[a] = arcHeat{a, bestT, best, sum / float64(T)}
	}
	sort.Slice(heats, func(i, j int) bool { return heats[i].v > heats[j].v })

	fmt.Printf("rank  arc    worst-slot  saturation      mean   from -> to\n")
	for i := 0; i < *top && i < len(heats); i++ {
		h := heats[i]
		fmt.Printf("%4d  %5d  %10d  %10.6f  %8.6f   %s -> %s\n", i+1, h.a, h.t, h.v, h.sum,
			inst.NodeNames[g.From[h.a]], inst.NodeNames[g.To[h.a]])
	}
}
