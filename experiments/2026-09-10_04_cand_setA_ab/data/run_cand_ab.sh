#!/usr/bin/env bash
# Three-arm candidate-layer benchmark, ordered INSTANCE-outer so that every
# instance produces a complete A/B/C triple before the next one starts.
#
#   A legacy : -cand-mode off                             (HEAD-equivalent path)
#   B cand   : -cand-mode mix -cand-hop-cache=true        (cached hop BFS)
#   C nocache: -cand-mode mix -cand-hop-cache=false       (uncached control arm)
#
# Why instance-outer: -wall-sec is only checked *between* rounds, and a single
# MIP round on the hard instances (setA-16/19) has been observed to run 30+
# minutes.  With arm-outer ordering a slow arm A would block arms B and C
# entirely; instance-outer at least yields full triples for the instances that
# finish, and cmd/bench resumes by skipping rows already present in summary.tsv.
#
# Instances are ordered cheapest-first so the readable part of the table fills in
# early; the two 4000-layer instances are last.
set -u
cd "$(dirname "$0")/.."

# Two concurrent runs sharing one output dir corrupt each other's summary.tsv
# (both append, and the resume-by-skip logic then trusts the mixed rows).  The
# previous attempt hit exactly that, so refuse to start if one is already up.
if ps -W 2>/dev/null | grep -qi "bench_cand\|solve_cand"; then
  echo "refusing to start: a bench_cand/solve_cand process is already running" >&2
  exit 1
fi

ONLY=${ONLY:-01,04,06,10,13,15,16,19}
WALL=${WALL:-90}

for inst in ${ONLY//,/ }; do
  for arm in A B C; do
    case $arm in
      A) M=off; HC=true  ;;
      B) M=mix; HC=true  ;;
      C) M=mix; HC=false ;;
    esac
    echo "########## setA-$inst  arm $arm  mode=$M hop-cache=$HC ##########"
    ./_pyref/bench_cand.exe \
      -bin ./_pyref/solve_cand.exe \
      -dir _pyref/cand_ab/$arm \
      -only "$inst" \
      -wall-sec "$WALL" \
      -cand-mode $M \
      -cand-hop-cache=$HC
  done
done
echo "ALL ARMS DONE"
