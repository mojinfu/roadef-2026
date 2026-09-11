#!/usr/bin/env bash
# Full setB sweep: every instance under both schedules, one at a time, capped at
# 10 minutes of wall clock each.  Serial on purpose -- the Gurobi license and the
# cores are shared, so a parallel sweep would not be an equal-budget comparison.
#
# A finished instance is skipped on a rerun (its solution JSON exists), so the
# sweep can be resumed after an interruption without redoing the whole 4 hours.
set -u
ROOT=/d/code/challenge-roadef-2026
OUT=$ROOT/solution/runs_setB_full
BIN=${BIN:-/tmp/tasr-solve.exe}
WALL=${WALL:-600}
cd "$ROOT" || exit 1

for inst in $(seq -w 1 12); do
	for arm in hot pingpong; do
		d=$OUT/$arm
		mkdir -p "$d"
		sol=$d/setB-$inst.json
		log=$d/setB-$inst.log
		if [ -f "$sol" ]; then
			echo "skip $arm setB-$inst (already done)"
			continue
		fi
		echo "=== $arm setB-$inst  start $(date +%H:%M:%S) ==="
		"$BIN" -prefix "setB/setB-$inst" -schedule "$arm" \
			-wall-sec "$WALL" -rounds 100000 -monitor=false \
			-out "$sol" >"$log" 2>&1
		grep -a "^RESULT" "$log" || echo "  !! no RESULT for $arm setB-$inst"
		echo "    done $(date +%H:%M:%S)"
	done
done
echo "ALL DONE"
