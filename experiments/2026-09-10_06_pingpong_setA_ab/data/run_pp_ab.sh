#!/bin/bash
# Two-arm pingpong A/B over setA, one bench process at a time.
#
# Why the wrapper exists: the first attempt ended up with two bench processes
# against the same summary.tsv (duplicate rows) and let a killed parent orphan a
# multi-GB solve.exe.  So this driver  (a) refuses to start if any bench/solve is
# already alive,  (b) runs one instance at a time and retries the ones that came
# back without a row,  (c) kills any orphaned solve.exe after every attempt.
#
# usage: run_pp_ab.sh <armdir> <schedule> <conf-gate> <logfile>
set -u
ROOT=/d/code/challenge-roadef-2026/solution
cd "$ROOT" || exit 1
ARM=$1; SCHED=$2; GATE=$3; LOG=$4
BENCH=./_pyref/bench_pp.exe
INSTS=""

busy() { tasklist //FI "IMAGENAME eq bench_pp.exe" //FO CSV //NH 2>/dev/null | grep -q bench_pp; }

if busy; then echo "REFUSING: another bench_pp.exe is already running" >>"$LOG"; exit 2; fi

attempt=0
while :; do
  attempt=$((attempt+1))
  missing=""
  for n in $(seq -w 1 20); do
    grep -q "^setA-$n	" "$ARM/summary.tsv" 2>/dev/null || missing="$missing,$n"
  done
  missing=${missing#,}
  [ -z "$missing" ] && { echo "DONE arm=$ARM after $attempt attempts" >>"$LOG"; break; }
  [ "$attempt" -gt 6 ] && { echo "GAVE UP arm=$ARM missing=$missing" >>"$LOG"; break; }
  echo "== $ARM attempt $attempt missing=$missing" >>"$LOG"
  $BENCH -bin ./bin/solve.exe -dir "$ARM" -wall-sec 120 -rounds 400 \
         -schedule "$SCHED" -conf-gate="$GATE" -only "$missing" >>"$LOG" 2>&1
  # A killed parent can leave its child behind holding GBs; never let it survive
  # into the next attempt (that is what starved the instance after it).
  taskkill //F //IM solve.exe >/dev/null 2>&1
  sleep 3
done
