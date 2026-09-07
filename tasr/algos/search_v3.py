"""Solver v0.3: v0.2 move families augmented with distance-derived candidates.

v0.2's per-demand waypoint pool is a *uniform random* sample of nodes; most
evaluated nodes sit far from the demand's corridors.  v0.3 keeps the full
random pool (spreading-dominated instances need long, random detours that no
cheap corridor can express) and *adds* the corridor of the demand's
currently-hot segment legs on top, harvested from the same shortest-path
distances the ECMP computation already builds (see :mod:`tasr.algos.candidates`):

* ``slack ~ 0`` nodes of a hot leg lie on its shortest-path DAG and re-weight
  the ECMP split inside the current corridor (tail balancing);
* small positive slack nodes are cheap detours that can pull a demand off an
  arc that every shortest path crosses.

Best-improvement over the union costs a little more per sweep but can only see
a superset of v0.2's moves.  Everything else about the search is v0.2.
"""
from __future__ import annotations

import random
import time
from typing import List, Optional, Tuple

import numpy as np

from ..ecmp import AtomCache
from ..eval.evaluator import Evaluator
from ..eval.objective import CompareSpec, lex_compare_sorted, saturations_vector
from ..model import DirectedGraph, Instance, Solution
from .candidates import DistanceIndex, leg_corridor
from .search import SearchState
from .search_v2 import (
    SearchStateV2,
    _candidate_demands,
    _candidate_slots,
    _one_node_edits,
)

__all__ = ["local_search_v3", "solve_with_time_budget_v3"]

_POOL = 60            # uniform-random candidate nodes (v0.2 baseline pool)
_CORRIDOR_EXTRA = 30  # corridor nodes of the hot legs added on top
_N_HOT_SLOT = 8       # hottest arcs per slot whose carriers get targeted corridors
_DETOUR_FRAC = 0.5    # max corridor detour: slack <= rel_detour * dist(u, v)


# ---------------------------------------------------------------------------
# candidate pool
# ---------------------------------------------------------------------------
def _random_pool(state: SearchState, rng: random.Random, d: int) -> List[int]:
    dem = state.inst.demands[d]
    nodes = [w for w in range(state.g.n_nodes)
             if w not in (dem.source, dem.target)]
    if len(nodes) <= _POOL:
        return nodes
    rng.shuffle(nodes)
    return nodes[:_POOL]


def _slot_top_arcs(state: SearchState, t: int, n: int = _N_HOT_SLOT) -> List[int]:
    """Arc ids with the ``n`` highest saturations in column ``t``."""
    order = np.argsort(-state.sat[:, t], kind="stable")
    return [int(a) for a in order[:n]]


def _candidate_pool(
    state: SearchStateV2,
    idx: DistanceIndex,
    d: int,
    rng: random.Random,
    t: Optional[int] = None,
) -> List[int]:
    """Waypoint nodes for demand ``d``: random pool + corridor of its hot legs.

    ``t=None`` (twin move) targets corridors from both slots; otherwise only
    slot ``t`` is considered.  The uniform-random pool (as v0.2) is kept
    intact -- long detours on spreading instances are only reachable there --
    and the corridor nodes of legs that currently cross the slot's most
    saturated arcs are appended (deduplicated).  More candidates per sweep
    costs slightly more time but the best-improvement sweep sees a strict
    superset of v0.2's moves.
    """
    dem = state.inst.demands[d]
    pool = _random_pool(state, rng, d)
    if len(pool) < state.g.n_nodes - 2:
        # random pool was sampled, not exhaustive: corridor extras add value
        slots = (0, 1) if t is None else (t,)
        hot: set = set()
        for tt in slots:
            hot.update(_slot_top_arcs(state, tt))
        if not hot:
            return pool

        seen: set = set(pool)
        scored: List[Tuple[float, int]] = []
        for tt in slots:
            base = state.wp[d][tt]
            seq = (dem.source,) + tuple(base) + (dem.target,)
            for u, v in zip(seq[:-1], seq[1:]):
                if u == v:
                    continue
                atom = state.cache.atom(u, v, tt)
                if atom is None:
                    continue
                if not any(atom[a] > 0.0 for a in hot):
                    continue
                for rel, C in leg_corridor(idx, u, v, tt, _DETOUR_FRAC,
                                           exclude=seen):
                    seen.add(C)
                    scored.append((rel, C))
        scored.sort(key=lambda x: x[0])
        for _, C in scored[:_CORRIDOR_EXTRA]:
            if C not in pool:
                pool.append(C)
    return pool


# ---------------------------------------------------------------------------
# move families (each returns a move descriptor, never mutates the state)
# ---------------------------------------------------------------------------
def _best_twin(state: SearchStateV2, idx: DistanceIndex, rng: random.Random):
    """Best budget-free improving twin ``(d, wps)`` over one-node edits."""
    spec, kmax = state.spec, state.kmax()
    v_cur = state.vector()
    best = None
    for d in _candidate_demands(state):
        base = state.wp[d][0]
        pool = _candidate_pool(state, idx, d, rng)
        for nw in _one_node_edits(base, pool, kmax):
            f0 = state.path_flow(d, 0, nw)
            f1 = state.path_flow(d, 1, nw)
            if f0 is None or f1 is None:
                continue
            ns = state.sat.copy()
            ns[:, 0] += (f0 - state.demand_flow(d, 0)) / state.cap
            ns[:, 1] += (f1 - state.demand_flow(d, 1)) / state.cap
            vn = saturations_vector(ns)
            if lex_compare_sorted(vn, v_cur, spec.decimals) != -1:
                continue
            if best is None or lex_compare_sorted(vn, best[0], spec.decimals) == -1:
                best = (vn, d, nw)
    return best[1:] if best else None


def _best_single(state: SearchStateV2, idx: DistanceIndex, rng: random.Random):
    """Best budget-respecting improving single-slot move ``(d, t, wps)``."""
    spec, kmax = state.spec, state.kmax()
    v_cur = state.vector()
    best = None
    for d, t in _candidate_slots(state):
        base = state.wp[d][t]
        pool = _candidate_pool(state, idx, d, rng, t=t)
        for nw in _one_node_edits(base, pool, kmax):
            if state.budget1 is not None:
                nd = state.distance_after_single(d, t, nw)
                if state.distance() - state._demand_dist(d) + nd > state.budget1:
                    continue
            newf = state.path_flow(d, t, nw)
            if newf is None:
                continue
            ns = state.sat.copy()
            ns[:, t] += (newf - state.demand_flow(d, t)) / state.cap
            vn = saturations_vector(ns)
            if lex_compare_sorted(vn, v_cur, spec.decimals) != -1:
                continue
            if best is None or lex_compare_sorted(vn, best[0], spec.decimals) == -1:
                best = (vn, d, t, nw)
    return best[1:] if best else None


def _best_shift(state: SearchStateV2, idx: DistanceIndex, rng: random.Random):
    """Coordinated ``(d_twin, twin_wps, d_spend, t_spend, wps)`` -- as in v0.2."""
    if state.budget1 is None or state.distance() == 0:
        return None
    spec, kmax = state.spec, state.kmax()
    divergent = [d for d in range(state.D) if state._demand_dist(d) > 0]
    if not divergent:
        return None
    v_cur = state.vector()
    best = None
    for d_twin in divergent:
        freed = state._demand_dist(d_twin)
        twin_wps = state.wp[d_twin][0]
        if state.path_flow(d_twin, 1, twin_wps) is None:
            twin_wps = state.wp[d_twin][1]
            if state.path_flow(d_twin, 0, twin_wps) is None:
                continue
        f0 = state.path_flow(d_twin, 0, twin_wps)
        f1 = state.path_flow(d_twin, 1, twin_wps)
        pre = state.sat.copy()
        pre[:, 0] += (f0 - state.demand_flow(d_twin, 0)) / state.cap
        pre[:, 1] += (f1 - state.demand_flow(d_twin, 1)) / state.cap
        for d_spend, t_spend in _candidate_slots(state):
            if d_spend == d_twin:
                continue
            base = state.wp[d_spend][t_spend]
            pool = _candidate_pool(state, idx, d_spend, rng, t=t_spend)
            for nw in _one_node_edits(base, pool, kmax):
                nd = state.distance_after_single(d_spend, t_spend, nw)
                if state.distance() - freed - state._demand_dist(d_spend) + nd \
                        > state.budget1:
                    continue
                newf = state.path_flow(d_spend, t_spend, nw)
                if newf is None:
                    continue
                ns = pre.copy()
                ns[:, t_spend] += (newf - state.demand_flow(d_spend, t_spend)) / state.cap
                vn = saturations_vector(ns)
                if lex_compare_sorted(vn, v_cur, spec.decimals) != -1:
                    continue
                if best is None or lex_compare_sorted(vn, best[0], spec.decimals) == -1:
                    best = (vn, d_twin, twin_wps, d_spend, t_spend, nw)
    return best[1:] if best else None


# ---------------------------------------------------------------------------
# driver
# ---------------------------------------------------------------------------
def local_search_v3(state: SearchStateV2, idx: DistanceIndex,
                    rng: random.Random, max_seconds: float = 120.0) -> None:
    """Best-improvement descent over twin / single / shift (v0.3 candidates)."""
    t0 = time.time()
    while time.time() - t0 < max_seconds:
        mv = _best_twin(state, idx, rng)
        if mv is not None:
            d, wps = mv
            state.apply_twin(d, wps)
            continue

        mv = _best_single(state, idx, rng)
        if mv is not None:
            d, t, wps = mv
            state.apply_single(d, t, wps)
            continue

        mv = _best_shift(state, idx, rng)
        if mv is not None:
            d_twin, twin_wps, d_spend, t_spend, wps = mv
            state.apply_twin(d_twin, twin_wps)
            state.apply_single(d_spend, t_spend, wps)
            continue

        break  # converged


def solve_with_time_budget_v3(
    inst: Instance,
    graph: DirectedGraph,
    cache: AtomCache,
    time_budget: float = 30.0,
    restarts: int = 3,
    seed: int = 0,
) -> Tuple[Solution, SearchStateV2, np.ndarray]:
    """Multi-start v0.3 local search; returns (best Solution, state, sat)."""
    rng = random.Random(seed)
    base_sat = Evaluator(inst, graph, cache).saturations(
        Solution.empty(inst.n_demands, inst.n_slots)).copy()
    idx = DistanceIndex(graph, inst.scenario.interventions)
    best_state: Optional[SearchStateV2] = None
    best_vec: Optional[np.ndarray] = None
    per_restart = time_budget / max(restarts, 1)

    for r in range(restarts):
        state = SearchStateV2(inst, graph, cache)
        state.sat = base_sat.copy()
        state.recompute()
        local_search_v3(state, idx, random.Random(seed + 997 * r),
                        max_seconds=per_restart)
        v = state.vector()
        if best_vec is None or lex_compare_sorted(v, best_vec, state.spec.decimals) == -1:
            best_state, best_vec = state, v

    assert best_state is not None
    sol = Solution.empty(inst.n_demands, inst.n_slots)
    for d in range(inst.n_demands):
        for t in range(inst.n_slots):
            sol.set_waypoints(d, t, best_state.wp[d][t])
    return sol, best_state, best_state.sat
