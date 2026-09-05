"""Correctness invariants of the v0.2 multi-segment local search."""
from __future__ import annotations

import numpy as np
import pytest

from tasr.algos.search_v2 import SearchStateV2, local_search_v2, solve_with_time_budget_v2
from tasr.ecmp import AtomCache
from tasr.eval import Evaluator
from tasr.eval.objective import saturations_vector
from tasr.io import load_instance
from tasr.model import DirectedGraph, Solution

REPO = __import__("pathlib").Path(__file__).resolve().parents[1]


def _inst(name: str):
    pre = f"{REPO}/setA/{name}"
    return load_instance(f"{pre}-net.json", f"{pre}-tm.json",
                         f"{pre}-scenario.json", name=name)


@pytest.mark.parametrize("name", ["setA-01", "setA-06"])
def test_sat_and_dist_consistent_after_search(name):
    """After v0.2 descent, incremental sat == full recompute and distance matches."""
    inst = _inst(name)
    g = DirectedGraph.build(inst)
    cache = AtomCache(g, inst.scenario.interventions, maxsize=100_000)
    state = SearchStateV2(inst, g, cache)
    state.sat = Evaluator(inst, g, cache).saturations(
        Solution.empty(inst.n_demands, inst.n_slots)).copy()
    state.recompute()
    import random
    local_search_v2(state, random.Random(0), max_seconds=3.0)
    # incremental sat must equal a full recompute from the stored waypoints
    sat_inc = state.sat.copy()
    sat2 = state.recompute()
    assert np.abs(sat2 - sat_inc).max() < 1e-9
    d2 = sum(len(set(_seg_pairs(state.wp[d][0], inst.demands[d].source,
                                inst.demands[d].target)) ^ set(
        _seg_pairs(state.wp[d][1], inst.demands[d].source, inst.demands[d].target)))
        for d in range(inst.n_demands))
    assert state.distance() == d2
    if state.budget1 is not None:
        assert state.distance() <= state.budget1
    # multi-waypoint paths never exceed the segment limit
    for d in range(inst.n_demands):
        for t in range(inst.n_slots):
            assert len(state.wp[d][t]) + 1 <= inst.scenario.max_segments


def _seg_pairs(wps, s, t):
    seq = (s,) + tuple(wps) + (t,)
    return [(u, w) for u, w in zip(seq[:-1], seq[1:]) if u != w]


def test_v2_improves_tight_budget_twin_case():
    """setA-10 has budget 1 (pure-twin): v0.2 must beat the single-waypoint
    twin ceiling of ~0.57 reached by v0.1 within a few seconds."""
    inst = _inst("setA-10")
    g = DirectedGraph.build(inst)
    cache = AtomCache(g, inst.scenario.interventions, maxsize=200_000)
    sol, state, sat = solve_with_time_budget_v2(inst, g, cache,
                                                time_budget=8.0, restarts=1, seed=0)
    assert state.distance() == 0
    mlu = float(sat.max())
    assert mlu < 0.55  # below v0.1's ~0.576 result (strictly better path spread)
    # the returned Solution reproduces the same saturations
    ev = Evaluator(inst, g, cache)
    check = ev.saturations(sol)
    assert np.abs(check - sat).max() < 1e-9
