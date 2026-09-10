#!/bin/bash
# setB-01 has no sprint reference, so the comparison is halo-off vs halo-on
# directly on the truncated-lex vector.  Same wall budget for both arms; the
# round count each arm fits is reported so the wall-clock confound is visible.
BIN=./runs_diag/solve_sticky.exe
echo "== arm halo=on (live page at http://127.0.0.1:8765/) =="
$BIN -prefix ../setB/setB-01 -out runs_diag/b01_on.json -rounds 400 -wall-sec 180 \
     -monitor=true > runs_diag/b01_on.log 2>&1
grep -o 'solves=[0-9]*' runs_diag/b01_on.log | tail -1
grep -o 'RESULT .*' runs_diag/b01_on.log | tail -1
grep -o 'halo: rolled on .*' runs_diag/b01_on.log | tail -1
echo "== arm halo=off =="
$BIN -prefix ../setB/setB-01 -out runs_diag/b01_off.json -rounds 400 -wall-sec 180 \
     -monitor=false -halo=false > runs_diag/b01_off.log 2>&1
grep -o 'solves=[0-9]*' runs_diag/b01_off.log | tail -1
grep -o 'RESULT .*' runs_diag/b01_off.log | tail -1
echo B01_AB_DONE
