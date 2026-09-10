#!/bin/bash
# Halo size-budget sweep.  The halo cap is -halo-mult x the base pool's demand
# count, applied on two axes (radiation cells targeted, demands admitted).  This
# measures what cutting it costs in quality and saves in wall-clock rounds.
# Deterministic: seed=0, fixed round set; a run not wall-limited repeats byte-for-byte.
BIN=./bin/solve_mult.exe
for inst in setA-01 setA-08 setA-17 setA-15; do
  for m in 1 2 3; do
    $BIN -prefix ../setA/$inst -sprint ../sprint_results/loads_vector.csv \
         -out runs_mult/${inst}_m${m}.json -rounds 400 -wall-sec 60 \
         -monitor=false -halo-mult $m > runs_mult/${inst}_m${m}.log 2>&1
    {
      printf "%-8s m=%s  " "$inst" "$m"
      grep -o 'solves=[0-9]* avg pool pairs=[0-9.]*' runs_mult/${inst}_m${m}.log | tail -1 | tr '\n' ' '
      grep -o 'hops queries=[0-9]*' runs_mult/${inst}_m${m}.log | tail -1 | tr '\n' ' '
      grep -o 'fired on [0-9]*/[0-9]* rounds, +[0-9]* pairs' runs_mult/${inst}_m${m}.log | tail -1 | tr '\n' ' '
      grep -o 'tie [0-9]*/[0-9]* layers; first gap layer [0-9]* ([0-9]* vs [0-9]*)' runs_mult/${inst}_m${m}.log | tail -1
      echo
    } >> runs_mult/summary.txt
  done
done
echo SWEEP_DONE
