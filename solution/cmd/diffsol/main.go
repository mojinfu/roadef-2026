// Command diffsol compares two srpaths solutions of one instance by the
// official truncated-lex saturation vector (truncate to 6 decimals, sorted
// descending, lexicographic — smaller is better).  It reports the first
// divergent layer with both quantised values and, over the whole vector, how
// many layers each solution wins.  This is the A/B yardstick for pool-model or
// scheduling changes (e.g. twin vs sticky) at equal wall budget.
package main

import (
	"flag"
	"fmt"
	"os"

	"tasr/internal/ecmp"
	"tasr/internal/eval"
	"tasr/internal/graph"
	"tasr/internal/io"
	"tasr/internal/snap"
)

func main() {
	prefix := flag.String("prefix", "", "instance prefix, e.g. setA/setA-01 (adds -net/-tm/-scenario.json)")
	aPath := flag.String("a", "", "solution file A")
	bPath := flag.String("b", "", "solution file B")
	flag.Parse()
	if *prefix == "" || *aPath == "" || *bPath == "" {
		fmt.Fprintln(os.Stderr, "usage: diffsol -prefix setA/setA-01 -a solA.json -b solB.json")
		os.Exit(2)
	}

	inst, err := io.LoadInstance(*prefix)
	if err != nil {
		fatal(err)
	}
	g := graph.New(inst)
	cache := ecmp.NewCache(g, inst.Scenario.Blocked, 0)
	desc := func(path string) []float64 {
		sol, err := io.ReadSolution(path, inst)
		if err != nil {
			fatal(err)
		}
		sn, err := snap.New(inst, g, cache, sol)
		if err != nil {
			fatal(err)
		}
		return eval.SortedDesc(sn.Saturations())
	}
	ad, bd := desc(*aPath), desc(*bPath)

	ra, rb := eval.RankMatrix(ad), eval.RankMatrix(bd)
	layer, aQ, bQ := eval.FirstDivergence(ad, bd)
	if layer == 0 {
		fmt.Printf("FULL TIE across %d layers\n", len(ra))
		return
	}
	// Which solution wins and where; plus layer win/loss counts over the tail.
	aWins, bWins := 0, 0
	for i := range ra {
		switch {
		case ra[i] < rb[i]:
			aWins++
		case ra[i] > rb[i]:
			bWins++
		}
	}
	winner, loser, wQ, lQ := "A", "B", aQ, bQ
	if aQ > bQ {
		winner, loser, wQ, lQ = "B", "A", bQ, aQ
	}
	fmt.Printf("%s wins: first divergence layer %d (%s %d vs %s %d); full vector A-wins %d layers, B-wins %d layers\n",
		winner, layer, winner, wQ, loser, lQ, aWins, bWins)
	// Profile the first few divergent layers for context.
	shown := 0
	for i := 0; i < len(ra) && shown < 6; i++ {
		if ra[i] == rb[i] {
			continue
		}
		fmt.Printf("    layer %d: A %d vs B %d\n", i+1, ra[i], rb[i])
		shown++
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
