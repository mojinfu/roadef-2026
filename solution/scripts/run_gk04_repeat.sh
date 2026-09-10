#!/usr/bin/env bash
# Chained after run_gk_ab.sh: settle whether setA-04's G0 tie=10 / G12 tie=1 is a
# stable property of the candidate net or a search-trajectory accident.
#
# Why this matters: G0 and G12 differ by a strict superset of MIP columns, but the
# outer loop is a greedy accept-only trajectory, so a superset does NOT guarantee a
# lex-better final snapshot.  G12's layer-2 value is 580858 against the reference's
# 580857 -- one trunc6 unit, which lexicographically costs nine tie layers.  One
# pairing cannot tell "the extra columns hurt" from "the MIP landed elsewhere this
# once", so run three independent pairings.
#
# Each repeat gets its OWN -dir: bench_gk appends to summary.tsv and skips
# instances already recorded there, so a shared dir would produce exactly one row
# no matter how many times it is invoked.
set -u
cd "$(dirname "$0")/.."

# Wait for the in-flight sweep to drain.  A competing solve would not corrupt the
# metrics, but it would eat the shared wall-clock budget and make the remaining
# arms of run_gk_ab.sh look artificially slow.
while ps -W 2>/dev/null | grep -qiE "bench_gk|solve_gk"; do sleep 10; done
echo "sweep drained; starting setA-04 paired repeats"

for r in 1 2 3; do
  for gk in 0 12; do
    echo "########## repeat $r  setA-04  global-k=$gk ##########"
    ./_pyref/bench_gk.exe \
      -bin ./_pyref/solve_gk.exe \
      -dir "_pyref/gk_ab4/G${gk}_r${r}" \
      -only 04 \
      -wall-sec 90 \
      -cand-mode mix \
      -cand-hop-cache=true \
      -cand-global-k "$gk"
  done
done
echo "ALL REPEATS DONE"
