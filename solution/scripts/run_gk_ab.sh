#!/usr/bin/env bash
# Two-arm global-relief-net A/B, ordered INSTANCE-outer so every instance yields
# a complete G0/G12 pair before the next one starts (same reason as
# run_cand_ab.sh: -wall-sec is only checked between rounds, and a hard instance
# can spend 30+ minutes inside one MIP round).
#
#   G0  : -cand-mode mix -cand-global-k 0   (v1 candidate layer, no safety net)
#   G12 : -cand-mode mix -cand-global-k 12  (net restores the top-12 singletons
#                                            by relief over *all* nodes)
#
# GlobalK is orthogonal to -cand-mode and the net is appended after the
# round-robin merge, so G12 is a strict superset of G0's pool: any difference is
# attributable to the added nodes.
set -u
cd "$(dirname "$0")/.."

# Two concurrent runs sharing one output dir corrupt each other's summary.tsv
# (both append, and the resume-by-skip logic then trusts the mixed rows).
if ps -W 2>/dev/null | grep -qi "bench_gk\|solve_gk"; then
  echo "refusing to start: a bench_gk/solve_gk process is already running" >&2
  exit 1
fi

ONLY=${ONLY:-01,04,06,10,13,15,16,19}
WALL=${WALL:-90}

for inst in ${ONLY//,/ }; do
  for gk in 0 12; do
    echo "########## setA-$inst  global-k=$gk ##########"
    ./_pyref/bench_gk.exe \
      -bin ./_pyref/solve_gk.exe \
      -dir _pyref/gk_ab/G$gk \
      -only "$inst" \
      -wall-sec "$WALL" \
      -cand-mode mix \
      -cand-hop-cache=true \
      -cand-global-k "$gk"
  done
done
echo "ALL ARMS DONE"
