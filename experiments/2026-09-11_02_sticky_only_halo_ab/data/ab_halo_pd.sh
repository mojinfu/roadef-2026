#!/bin/bash
# Per-demand 30% halo, single seed=0 (reproducible).  The halo-off arm is
# unchanged and already measured (ab_<inst>_off.log); only the on arm is new.
BIN=./runs_diag/solve_pd.exe
for inst in setA-01 setA-02 setA-03 setA-05 setA-08 setA-11 setA-15 setA-17; do
  $BIN -prefix ../setA/$inst -sprint ../sprint_results/loads_vector.csv \
       -out runs_diag/pd_${inst}.json -rounds 400 -wall-sec 60 \
       -monitor=false > runs_diag/pd_${inst}.log 2>&1
  printf "%-9s %-36s rounds=%-5s %s\n" "$inst" \
    "$(grep -o 'tie .* layers; first gap layer [0-9]* ([0-9]* vs [0-9]*)' runs_diag/pd_${inst}.log | tail -1)" \
    "$(grep -o 'solves=[0-9]*' runs_diag/pd_${inst}.log | tail -1 | cut -d= -f2)" \
    "$(grep -o 'halo: expanded on [0-9]*/[0-9]* rounds, +[0-9]* pairs' runs_diag/pd_${inst}.log | tail -1)"
done
echo PD_SWEEP_DONE
