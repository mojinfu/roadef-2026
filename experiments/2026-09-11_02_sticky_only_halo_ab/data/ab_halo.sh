#!/bin/bash
# Halo A/B on the sticky model.  Deterministic: seed=0, fixed round set; a run
# that is not wall-clock-limited repeats byte-for-byte.
BIN=./runs_diag/solve_sticky.exe
for inst in setA-01 setA-02 setA-03 setA-05 setA-08 setA-11 setA-15 setA-17; do
  for arm in off on; do
    if [ "$arm" = off ]; then H=-halo=false; else H=-halo=true; fi
    $BIN -prefix ../setA/$inst -sprint ../sprint_results/loads_vector.csv \
         -out runs_diag/ab_${inst}_${arm}.json -rounds 400 -wall-sec 60 \
         -monitor=false $H > runs_diag/ab_${inst}_${arm}.log 2>&1
    printf "%-8s %-4s %s\n" "$inst" "$arm" "$(grep -o 'tie .* layers; first gap layer [0-9]* ([0-9]* vs [0-9]*)' runs_diag/ab_${inst}_${arm}.log | tail -1)"
  done
done
echo SWEEP_DONE
