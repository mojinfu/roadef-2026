# Set A benchmark — solver v0.1 (2026-09-05)

Engine: single-waypoint (+empty) local search, multi-restart, incremental saturation,
lexicographic trunc6 objective.  Time budget 20 s/instance, 2 restarts.  Full table in
`runs_benchA.log`; solutions in `runs/setA-*-srpaths.json`.

## Results vs the empty (pure-ECMP) baseline

| Group | Instances | Outcome |
|---|---|---|
| Top-1 load reduced to the sprint-best value | setA-01 02 03 07 09 11 15 20 | our truncated max load == recorded sprint-best max load (6 decimals) |
| Top-1 already at the floor (≈1.0 unavoidable) | setA-18 | max ≈0.999998, matches sprint-best; can't do better |
| Far behind sprint-best even on max load | setA-04 05 06 08 10 13 14 16 17 19 | our max 0.2–0.95 vs sprint 0.04–0.58 |
| Whole vector | all but setA-12 | strictly worse than sprint-best from the 1st or 2nd entry onward |

## Conclusions

* The io/model/ECMP/evaluator/search stack produces feasible-looking solutions for all
  20 instances in seconds; inter-slot distance stays within budget in every run.
* v0.1's reach is exactly what a single-waypoint neighbourhood can do: on instances whose
  optimum is dominated by one unavoidable heavy arc it ties the known best max load; where
  the optimum needs load spread across many corridors (multi-segment routing) it stalls far
  from the reference.

## Known problem: setA-12 "BETTER" is not trustworthy

`vs_best = BETTER` on setA-12 means our solution's vector is lexicographically smaller than
the sprint's recorded best (S22).  But under our evaluator the *empty baseline* (0.680018)
is also better than the recorded 0.879872, and S22 was rank 1 (strictly best of 53 teams).
That is only coherent if our evaluator under-estimates congestion on setA-12 relative to the
official checker.  Mitigations explored: ECMP tight-arc tolerance 1e-9 → 1e-6 leaves the
baseline unchanged, so the discrepancy is not the tolerance.  **Do not rely on any numeric
"better-than-sprint" claim until loads are cross-validated against the official C++ checker.**

## Next steps

1. Cross-validate load numbers vs official checker (Docker/WSL) — gating risk (task #8).
2. v0.2 solver: multi-segment paths + coordinated/penalty moves for the vector tail and the
   far-behind instances (task #9); then re-run with the real 10 min/instance budget.
