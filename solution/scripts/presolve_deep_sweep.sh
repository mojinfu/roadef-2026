#!/usr/bin/env bash
# Sweep the class-3 (deep) pass cap `-presolve-deep` on every setA instance and
# record what the cap costs (elapsed) versus what it loses (floors, LB_sat).
#
# The deep pass sorts cells by observed saturation descending, so the question
# this answers is: does a cap of N cells keep the same LB_sat as the uncapped
# run, and how much of the 15s budget does it save?  All runs use `-rounds 0`,
# i.e. the decompose loop never starts, so no Gurobi model is built.
set -u
cd "$(dirname "$0")/.." || exit 1
BIN=${BIN:-runs_sticky_ab/solve_pres.exe}
OUT=${OUT:-../experiments/2026-09-10_02_presolve_setA_ab/data/presolve_deep_sweep.tsv}
DEPTHS="${DEPTHS:-0 100 300 600 1200}"
SETS="${SETS:-setA}"

printf 'instance\tmaxdeep\telapsed\tcells\tdeep\tfloors\tproven\tlb_rank\n' >"$OUT"
for s in $SETS; do
for i in $(seq -w 1 20); do
  inst="$s-$i"
  [ -f "../$s/$inst-net.json" ] || continue
  for d in $DEPTHS; do
    log=$(MSYS_NO_PATHCONV=1 "$BIN" -prefix "../$s/$inst" -out "/tmp/pds-$inst.json" \
      -rounds 0 -presolve unmovable -presolve-deep "$d" 2>&1 | tr '\0' '\n')
    line=$(printf '%s\n' "$log" | grep '^presolve:' | head -1)
    lb=$(printf '%s\n' "$log" | grep 'lb_sat=' | head -1)
    elapsed=$(sed -E 's/^presolve: ([0-9.]+)s.*/\1/' <<<"$line")
    cells=$(sed -E 's/.*cells=([0-9]+).*/\1/' <<<"$line")
    deep=$(sed -E 's/.*deep=([0-9]+).*/\1/' <<<"$line")
    floors=$(sed -E 's/.*(floors|pinned)=([0-9]+).*/\2/' <<<"$line")
    proven=$(sed -E 's/.*proven=([0-9]+).*/\1/' <<<"$line")
    lbrank=$(sed -E 's/.*first-bit rank ([0-9]+).*/\1/' <<<"$lb")
    printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
      "$inst" "$d" "$elapsed" "$cells" "$deep" "$floors" "$proven" "$lbrank" >>"$OUT"
    echo "  $inst deep=$d elapsed=${elapsed}s floors=$floors lb=$lbrank"
  done
done
done
echo "wrote $OUT"
