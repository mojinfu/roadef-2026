"""Correctness invariants of the v0.3 distance-derived candidate search."""
from __future__ import annotations

import random

import numpy as np
import pytest

from tasr.algos.candidates import DistanceIndex, leg_corridor
from tasr.algos.search_v2 import SearchStateV2, _candidate_demands
from tasr.algos.search_v3 import (
    local_search_v3,
    solve_with_time_budget_v3,
    _candidate_pool,
    _POOL,
)
from tasr.ecmp import AtomCache
from tasr.eval import Evaluator
from tasr.io import load_instance
from tasr.model import DirectedGraph, Solution

from .toy import build_toy_instance

REPO = __import__("pathlib").Path(__file__).resolve().parents[1]


def _inst(name: str):
    pre = f"{REPO}/setA/{name}"
    return load_instance(f"{pre}-net.json", f"{pre}-tm.json",
                         f"{pre}-scenario.json", name=name)


def _state(inst):
    g = DirectedGraph.build(inst)
    cache = AtomCache(g, inst.scenario.interventions, maxsize=100_000)
    state = SearchStateV2(inst, g, cache)
    state.sat = Evaluator(inst, g, cache).saturations(
        Solution.empty(inst.n_demands, inst.n_slots)).copy()
    state.recompute()
    return g, cache, state


def test_leg_corridor_geometry_toy():
    """Toy graph: for leg 0->5 (demand d0) the corridor nodes are the shortest-
    path DAG interior (slack 0) sorted before cheap detours."""
    inst = build_toy_instance()
    g = DirectedGraph.build(inst)
    idx = DistanceIndex(g, inst.scenario.interventions)
    corr = leg_corridor(idx, 0, 5, 0, rel_detour=1.0)
    nodes = [C for _, C in corr]
    assert 4 in nodes          # v4 on a shortest 0->5 path
    assert 1 in nodes          # v1 also on a shortest 0->5 path
    assert 0 not in nodes and 5 not in nodes
    # slack ascending: first entry is on a shortest path (rel slack 0)
    assert corr[0][0] < 1e-9


def test_candidate_pool_targets_demand_corridors():
    """On setA-01 the pool for a hot demand contains nodes on its corridor, is
    bounded by _POOL and excludes nothing that would break path_flow."""
    inst = _inst("setA-01")
    g, cache, state = _state(inst)
    idx = DistanceIndex(g, inst.scenario.interventions)
    rng = random.Random(0)
    d = next(iter(_candidate_demands(state)))
    pool = _candidate_pool(state, idx, d, rng)
    assert 0 < len(pool) <= _POOL
    assert len(set(pool)) == len(pool)
    for w in pool:
        # every proposed waypoint must be routable as a single-waypoint path
        assert state.path_flow(d, 0, (w,)) is not None


@pytest.mark.parametrize("name", ["setA-01", "setA-06"])
def test_sat_and_dist_consistent_after_search(name):
    """After v0.3 descent, incremental sat == full recompute, distance matches,
    multi-waypoint paths respect the segment limit."""
    inst = _inst(name)
    g = DirectedGraph.build(inst)
    cache = AtomCache(g, inst.scenario.interventions, maxsize=100_000)
    state = SearchStateV2(inst, g, cache)
    state.sat = Evaluator(inst, g, cache).saturations(
        Solution.empty(inst.n_demands, inst.n_slots)).copy()
    state.recompute()
    idx = DistanceIndex(g, inst.scenario.interventions)
    import random
    local_search_v3(state, idx, random.Random(0), max_seconds=3.0)
    sat_inc = state.sat.copy()
    sat2 = state.recompute()
    assert np.abs(sat2 - sat_inc).max() < 1e-9
    if state.budget1 is not None:
        assert state.distance() <= state.budget1
    for d in range(inst.n_demands):
        for t in range(inst.n_slots):
            assert len(state.wp[d][t]) + 1 <= inst.scenario.max_segments


def test_v3_improves_tight_budget_twin_case():
    """setA-10 has budget 1 (pure-twin): v0.3 must also break the ~0.576 single-
    waypoint ceiling and reproduce its saturations through the Solution."""
    inst = _inst("setA-10")
    g = DirectedGraph.build(inst)
    cache = AtomCache(g, inst.scenario.interventions, maxsize=200_000)
    sol, state, sat = solve_with_time_budget_v3(inst, g, cache,
                                                time_budget=8.0, restarts=1, seed=0)
    assert state.distance() == 0
    assert float(sat.max()) < 0.55
    ev = Evaluator(inst, g, cache)
    check = ev.saturations(sol)
    assert np.abs(check - sat).max() < 1e-9
