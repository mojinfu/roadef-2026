// Command validate loads a T-ASR instance (+ optional srpaths solution),
// evaluates the saturation matrix with the Go ECMP engine and reports
// statistics.  With -oracle it cross-checks the computation bit-for-bit against
// the reference Python oracle output (oracle_sat.py); with -dump it writes the
// same oracle text format for offline diffing.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"tasr/internal/ecmp"
	"tasr/internal/eval"
	"tasr/internal/graph"
	"tasr/internal/io"
	"tasr/internal/model"
)

func main() {
	prefix := flag.String("prefix", "", "instance prefix, e.g. setA/setA-01 (adds -net/-tm/-scenario.json)")
	srpaths := flag.String("srpaths", "", "optional solution file (default: empty solution)")
	oracle := flag.String("oracle", "", "reference oracle text file to compare against")
	dump := flag.String("dump", "", "write oracle-format output to this file")
	writeSol := flag.String("write-srpaths", "", "write the (effective) solution to this srpaths file")
	flag.Parse()

	if *prefix == "" {
		fmt.Fprintln(os.Stderr, "usage: validate -prefix setA/setA-01 [-srpaths sol.json] [-oracle out.txt] [-dump out.txt]")
		os.Exit(2)
	}

	inst, err := io.LoadInstance(*prefix)
	if err != nil {
		fatal(err)
	}
	g := graph.New(inst)
	cache := ecmp.NewCache(g, inst.Scenario.Blocked, 0)
	evaluator := eval.NewEvaluator(inst, g, cache)

	var sol *model.Solution
	if *srpaths == "" {
		sol = model.EmptySolution(inst.NDemands(), inst.NSlots)
	} else {
		sol, err = io.ReadSolution(*srpaths, inst)
		if err != nil {
			fatal(err)
		}
	}

	sat, err := evaluator.Saturations(sol)
	if err != nil {
		fatal(err)
	}

	// Statistics.
	m, T := g.M, inst.NSlots
	fmt.Printf("instance=%s nodes=%d arcs=%d slots=%d demands=%d\n",
		inst.Name, inst.NNodes(), m, T, inst.NDemands())
	for t := 0; t < T; t++ {
		maxSat := 0.0
		for a := 0; a < m; a++ {
			if sat[a*T+t] > maxSat {
				maxSat = sat[a*T+t]
			}
		}
		fmt.Printf("  slot %d: MLU=%.12f  (rank %d)\n", t, maxSat, eval.RankInt(maxSat))
	}

	// Optional dump / oracle comparison in the oracle text format.
	text := oracleText(sat, m, T, inst)
	if *dump != "" {
		if err := os.WriteFile(*dump, []byte(text), 0o644); err != nil {
			fatal(err)
		}
	}
	if *writeSol != "" {
		if err := io.WriteSolution(*writeSol, sol, inst); err != nil {
			fatal(err)
		}
		fmt.Printf("wrote solution -> %s\n", *writeSol)
	}
	if *oracle != "" {
		compareOracle(*oracle, text, sat, m, T)
	}
}

func oracleText(sat []float64, m, T int, inst *model.Instance) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "HDR m=%d n_slots=%d n_demands=%d n_nodes=%d\n", m, T, inst.NDemands(), inst.NNodes())
	for t := 0; t < T; t++ {
		for a := 0; a < m; a++ {
			v := sat[a*T+t]
			fmt.Fprintf(&sb, "A %d %d %.17g\n", t, a, v)
			fmt.Fprintf(&sb, "M %d %d %d\n", t, a, eval.RankInt(v))
		}
	}
	desc := eval.SortedDesc(sat)
	for i := range desc {
		fmt.Fprintf(&sb, "V %.17g\n", desc[i])
		fmt.Fprintf(&sb, "Q %d\n", eval.RankInt(desc[i]))
	}
	return sb.String()
}

func compareOracle(path, goText string, sat []float64, m, T int) {
	f, err := os.Open(path)
	if err != nil {
		fatal(err)
	}
	defer f.Close()

	type row struct {
		a, t int
		v    float64
		q    int64
	}
	var aRows, mRows []row
	var vDesc, qDesc []int64

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "A", "M":
			t, err1 := strconv.Atoi(fields[1])
			a, err2 := strconv.Atoi(fields[2])
			if err1 != nil || err2 != nil {
				fatal(fmt.Errorf("oracle row parse: %q", line))
			}
			if fields[0] == "A" {
				v, err := strconv.ParseFloat(fields[3], 64)
				if err != nil {
					fatal(err)
				}
				aRows = append(aRows, row{a: a, t: t, v: v})
			} else {
				q, _ := strconv.ParseInt(fields[3], 10, 64)
				mRows = append(mRows, row{a: a, t: t, q: q})
			}
		case "V":
			v, err := strconv.ParseFloat(fields[1], 64)
			if err != nil {
				fatal(err)
			}
			vDesc = append(vDesc, eval.RankInt(v))
		case "Q":
			q, _ := strconv.ParseInt(fields[1], 10, 64)
			qDesc = append(qDesc, q)
		}
	}
	if err := sc.Err(); err != nil {
		fatal(err)
	}

	badA, badM := 0, 0
	first := ""
	for _, r := range aRows {
		got := sat[r.a*T+r.t]
		if got != r.v {
			badA++
			if first == "" {
				first = fmt.Sprintf("A t=%d a=%d py=%.17g go=%.17g", r.t, r.a, r.v, got)
			}
		}
	}
	for _, r := range mRows {
		got := eval.RankInt(sat[r.a*T+r.t])
		if got != r.q {
			badM++
		}
	}

	desc := eval.RankMatrix(eval.SortedDesc(sat))
	badV := 0
	for i := range vDesc {
		if i >= len(desc) {
			badV++
			continue
		}
		if desc[i] != vDesc[i] {
			badV++
		}
	}

	fmt.Printf("oracle compare: A-rows=%d badA=%d | M-rows=%d badM=%d | V len=%d badV=%d\n",
		len(aRows), badA, len(mRows), badM, len(vDesc), badV)
	if first != "" {
		fmt.Println("  first raw mismatch:", first)
	}
	if badA == 0 && badM == 0 && badV == 0 && len(vDesc) == len(desc) {
		fmt.Println("  RESULT: bitwise parity with reference oracle OK")
	} else {
		fmt.Println("  RESULT: MISMATCH (see above)")
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
