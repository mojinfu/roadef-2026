#!/bin/bash
# Equal-rounds setB-01: the off arm did 234 rounds and the on arm 26 under the
# same 180s, so the wall-clock verdict is confounded.  Cap both at 26 rounds
# with -wall-sec 0 and the round count stops being a variable.
BIN=./runs_diag/solve_sticky.exe
for arm in on off; do
  if [ "$arm" = off ]; then H=-halo=false; else H=-halo=true; fi
  s=$SECONDS
  $BIN -prefix ../setB/setB-01 -out runs_diag/eqB01_${arm}.json -rounds 26 -wall-sec 0 \
       -monitor=false $H > runs_diag/eqB01_${arm}.log 2>&1
  printf "%-4s rounds=%-4s wall=%ss accepted=%s firstbit=%s\n" "$arm" \
    "$(grep -o 'solves=[0-9]*' runs_diag/eqB01_${arm}.log | tail -1 | cut -d= -f2)" \
    "$((SECONDS-s))" \
    "$(grep -o 'accepted=[0-9]*' runs_diag/eqB01_${arm}.log | tail -1 | cut -d= -f2)" \
    "$(grep -o 'firstbit=[0-9]*' runs_diag/eqB01_${arm}.log | tail -1 | cut -d= -f2)"
done
echo B01_EQ_DONE
