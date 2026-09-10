// Command bench runs the M3 outer-loop solver over every setA instance and
// writes a quantified per-instance summary (RESULT line + sprint compare) into
// a TSV.  Instances whose row already exists in the TSV are skipped, so the
// bench can be resumed after a wall-clock interruption.  Each instance runs as
// its own solve.exe child with an optional -wall-sec cap.
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	bin := flag.String("bin", "_pyref/solve.exe", "solver binary")
	dir := flag.String("dir", "_pyref/bench_m4", "output directory")
	rounds := flag.Int("rounds", 150, "rounds per instance")
	wallSec := flag.Int("wall-sec", 0, "wall-clock cap per instance in seconds (0 = none)")
	hotK := flag.Int("hot-k", 6, "hot cells per round")
	peel := flag.Int("peel", 6, "lex peel depth")
	model := flag.String("model", "twin", "solver pool model: twin or sticky")
	budgetMode := flag.String("budget-mode", "full", "solver Hamming budget mode: full or first_half")
	flag.Parse()

	if err := os.MkdirAll(*dir, 0o755); err != nil {
		fatal(err)
	}
	tsvPath := filepath.Join(*dir, "summary.tsv")
	doneRows := map[string]bool{}
	if f, err := os.Open(tsvPath); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := sc.Text()
			if i := strings.IndexByte(line, '\t'); i > 0 {
				doneRows[line[:i]] = true
			}
		}
		f.Close()
	}
	f, err := os.OpenFile(tsvPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fatal(err)
	}
	defer f.Close()
	header := "instance\taccepted\tfirstbit\ttotal_cost\tbudget_ok\ttie_layers\ttotal_layers\tfirst_gap_layer\tours\tref"
	if len(doneRows) == 0 {
		fmt.Fprintln(f, header)
	}

	for _, inst := range instanceList() {
		if doneRows[inst] {
			fmt.Printf("== %s (cached)\n", inst)
			continue
		}
		fmt.Printf("== %s ==\n", inst)
		start := time.Now()
		args := []string{
			"-prefix", "../setA/" + inst,
			"-out", filepath.Join(*dir, inst+".json"),
			"-sprint", "../sprint_results/loads_vector.csv",
			"-rounds", fmt.Sprint(*rounds),
			"-hot-k", fmt.Sprint(*hotK),
			"-peel", fmt.Sprint(*peel),
			"-model", *model,
			"-budget-mode", *budgetMode,
		}
		if *wallSec > 0 {
			args = append(args, "-wall-sec", fmt.Sprint(*wallSec))
		}
		cmd := exec.Command(*bin, args...)
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		_ = cmd.Run() // solver exit codes are not meaningful for the summary

		res, cmp := extract(&buf)
		line := row(inst, res, cmp)
		if _, err := fmt.Fprintln(f, line); err != nil {
			fatal(err)
		}
		doneRows[inst] = true
		fmt.Printf("  %s  (%.1fs)\n", line, time.Since(start).Seconds())
	}
}

func instanceList() []string {
	out := make([]string, 20)
	for i := range out {
		out[i] = fmt.Sprintf("setA-%02d", i+1)
	}
	return out
}

// extract pulls the "RESULT ..." and "sprint compare: ..." lines out of the
// solver output (which may be polluted by Gurobi NUL-separated logging).
func extract(buf *bytes.Buffer) (res, cmp string) {
	sc := bufio.NewScanner(buf)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if res == "" && strings.HasPrefix(line, "RESULT ") {
			res = line
		}
		if strings.HasPrefix(line, "sprint compare: ") {
			cmp = line
		}
	}
	return res, cmp
}

func row(inst, res, cmp string) string {
	field := func(pattern string) string {
		i := strings.Index(res, pattern)
		if i < 0 {
			return "NA"
		}
		rest := res[i+len(pattern):]
		j := strings.IndexAny(rest, " \t")
		if j < 0 {
			return rest
		}
		return rest[:j]
	}
	if res == "" {
		return fmt.Sprintf("%s\tNA\tNA\tNA\tNA\t0\tNA\t0\tNA\tNA", inst)
	}
	acc := field("accepted=")
	fb := field("firstbit=")
	tc := field("total_cost=")
	bo := field("budget_ok=")
	// cmp: tie 3/160 layers; first gap layer 4 (.. vs ..)
	tie, tot, gl, ours, ref := "0", "NA", "0", "NA", "NA"
	if cmp != "" {
		if i := strings.Index(cmp, "tie "); i >= 0 {
			mid := cmp[i+4:]
			if j := strings.IndexByte(mid, '/'); j >= 0 {
				tie = mid[:j]
			}
		}
		if i := strings.Index(cmp, "/"); i >= 0 {
			rest := cmp[i+1:]
			if j := strings.IndexAny(rest, " layer"); j >= 0 {
				tot = rest[:j]
			}
		}
		if i := strings.Index(cmp, "first gap layer "); i >= 0 {
			rest := cmp[i+len("first gap layer "):]
			if j := strings.IndexByte(rest, ' '); j >= 0 {
				gl = rest[:j]
				tail := rest[j+1:] // "(.. vs ..)"
				if k := strings.IndexByte(tail, '('); k >= 0 {
					body := tail[k+1:]
					if m := strings.Index(body, " vs "); m >= 0 {
						ours = body[:m]
						ref = strings.TrimRight(body[m+4:], ")")
					}
				}
			}
		}
	}
	return fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s",
		inst, acc, fb, tc, bo, tie, tot, gl, ours, ref)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
