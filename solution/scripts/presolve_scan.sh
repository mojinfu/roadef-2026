#!/usr/bin/env bash
# Probe the presolve phase alone (`-rounds 0` = decompose loop never runs) for
# every setA/setB instance and write one TSV row per instance:
#
#   instance  elapsed  cells  deep  floors  proven  revoked  timeout  lb_rank  start_rank
#
# Used by experiments/2026-09-10_02_presolve_setA_ab/report.md §2 to show which
# instances actually get floors and where the deep (class 3) pass eats its budget.
set -u
cd "$(dirname "$0")/.." || exit 1
BIN=${BIN:-runs_sticky_ab/solve_pres.exe}
OUT=${OUT:-../experiments/2026-09-10_02_presolve_setA_ab/data/presolve_report.tsv}
declare -A FIELD

printf 'instance\telapsed\tcells\tdeep\tfloors\tproven\trevoked\ttimeout\tlb_rank\tstart_rank\n' >"$OUT"
for set in setA setB; do
  for i in $(seq -w 1 20); do
    inst="$set-$i"
    [ -f "../$set/$inst-net.json" ] || continue
    log=$(MSYS_NO_PATHCONV=1 "$BIN" -prefix "../$set/$inst" \
      -out "/tmp/prescan-$inst.json" -sprint ../sprint_results/loads_vector.csv \
      -rounds 0 -presolve unmovable 2>&1 | tr '\0' '\n')
    line=$(printf '%s\n' "$log" | grep '^presolve:' | head -1)
    lb=$(printf '%s\n' "$log" | grep 'lb_sat=' | head -1)
    start=$(printf '%s\n' "$log" | grep 'start first-bit' | head -1)
    elapsed=$(sed -E 's/^presolve: ([0-9.]+)s.*/\1/' <<<"$line")
    cells=$(sed -E 's/.*cells=([0-9]+).*/\1/' <<<"$line")
    deep=$(sed -E 's/.*deep=([0-9]+).*/\1/' <<<"$line")
    floors=$(sed -E 's/.*(floors|pinned)=([0-9]+).*/\2/' <<<"$line")
    proven=$(sed -E 's/.*proven=([0-9]+).*/\1/' <<<"$line")
    revoked=$(sed -E 's/.*revoked=([0-9]+).*/\1/' <<<"$line")
    timeout=$(sed -E 's/.*timeout=([a-z]+).*/\1/' <<<"$line")
    lbrank=$(sed -E 's/.*first-bit rank ([0-9]+).*/\1/' <<<"$lb")
    srank=$(sed -E 's/.*rank=([0-9]+).*/\1/' <<<"$start")
    printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
      "$inst" "$elapsed" "$cells" "$deep" "$floors" "$proven" "$revoked" "$timeout" "$lbrank" "$srank" >>"$OUT"
    echo "  $inst  elapsed=${elapsed}s floors=$floors proven=$proven timeout=$timeout lb=$lbrank start=$srank"
  done
done
echo "wrote $OUT"
