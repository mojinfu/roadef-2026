"""Solver v0.2: multi-segment + coordinated moves on top of the v0.1 engine.

Motivation (see experiments/exp01)
----------------------------------
v0.1 (single-waypoint neighbourhood) plateaus far above the sprint reference on
tight-budget instances (setA-06/10/13/16/19, ``budget`` 1 or 13).  The inter-slot
budget is a *sum over demands* of the checker distance ``dist(path_{t-1}, path_t)``
(see ``tasr/algos/search.py``).  Two consequences drive v0.2:

* Routing the *same* path on both slots of a demand ("twin") costs distance 0,
  so on a tight budget almost all engineering must happen as twins; v0.1 only
  ever built single-waypoint twins.
* Reference max-loads as low as 0.04-0.07 need traffic spread across several
  corridors; one waypoint per demand cannot express that.  Paths must carry up
  to ``max_segments-1`` waypoints.

v0.2 drives the same incremental state (``SearchState``) with three move
families over waypoint tuples of any feasible length:

* ``twin``   -- set both slots of a demand to the *same* multi-waypoint path
  (budget-free).  The workhorse on tight budgets.
* ``single`` -- change one slot only (as v0.1), now with multi-waypoint
  candidates, gated by the pooled inter-slot budget.
* ``shift``  -- coordinated two-demand move: twin a currently-divergent demand
  (freeing its budget share) and spend that budget on a slot-1 reroute of a
  *different* demand that was previously budget-blocked.

The loop is a best-improvement descent.  Per sweep the neighbourhood is capped
so a sweep costs ~0.1-0.3 s regardless of the instance size:

* the demands (or demand/slot pairs) examined are those flowing through the
  *hottest* (arc, slot) positions, added bottleneck-first so the current
  bottleneck is never crowded out by larger flows on warmer arcs;
* the waypoint pool per demand is a fresh uniform random sample of ``_POOL``
  nodes (pure-random corridors beat local detours around the congested arcs).
"""
from __future__ import annotations

import random
import time
from typing import List, Optional, Tuple

import numpy as np

from ..ecmp import AtomCache
from ..eval.objective import lex_compare_sorted, saturations_vector
from ..model import DirectedGraph, Instance, Solution
from .search import SearchState

__all__ = ["SearchStateV2", "local_search_v2", "solve_with_time_budget_v2"]

_POOL = 60    # candidate waypoint nodes sampled per demand per sweep
_MAX_DT = 30  # (demand, slot) pairs examined per best-improvement sweep


# ---------------------------------------------------------------------------
# neighbourhood helpers
# ---------------------------------------------------------------------------
def _top_slots(state: SearchState, n: int) -> List[Tuple[int, int]]:
    """The ``n`` most-loaded (arc, slot) positions, desc, ties stable."""
    order = np.argsort(-state.sat, axis=None, kind="stable")
    _, h = state.sat.shape
    return [(int(idx // h), int(idx % h)) for idx in order[:n]]


def _node_pool(state: SearchState, rng: random.Random, d: int) -> List[int]:
    """Up to ``_POOL`` random candidate waypoint nodes for demand ``d``."""
    dem = state.inst.demands[d]
    nodes = [w for w in range(state.g.n_nodes)
             if w not in (dem.source, dem.target)]
    if len(nodes) <= _POOL:
        return nodes
    rng.shuffle(nodes)
    return nodes[:_POOL]


def _one_node_edits(base: Tuple[int, ...], pool: List[int], kmax: int) -> List:
    """Waypoint tuples reachable from ``base`` by one insert / replace / drop."""
    out = set()
    if len(base):
        for pos in range(len(base)):
            out.add(base[:pos] + base[pos + 1:])
    for w in pool:
        if len(base) < kmax:
            for pos in range(len(base) + 1):
                out.add(base[:pos] + (w,) + base[pos:])
        for pos in range(len(base)):
            out.add(base[:pos] + (w,) + base[pos + 1:])
    out.discard(tuple(base))
    return list(out)


def _candidate_demands(state: SearchState) -> List[int]:
    """Demands on the hottest (arc, slot) positions, bottleneck first."""
    seen, out = set(), []
    for a, t in _top_slots(state, 8):
        rows = [(state.demand_flow(d, t)[a], d) for d in range(state.D)]
        rows.sort(reverse=True)
        for c, d in rows:
            if c <= 1e-12 or d in seen:
                continue
            seen.add(d)
            out.append(d)
            if len(out) >= _MAX_DT:
                return out
    return out or list(range(min(state.D, _MAX_DT)))


def _candidate_slots(state: SearchState) -> List[Tuple[int, int]]:
    """(d, t) pairs flowing through the hottest positions, bottleneck first."""
    seen, out = set(), []
    for a, t in _top_slots(state, 8):
        rows = [(state.demand_flow(d, t)[a], d) for d in range(state.D)]
        rows.sort(reverse=True)
        for c, d in rows:
            if c <= 1e-12 or (d, t) in seen:
                continue
            seen.add((d, t))
            out.append((d, t))
            if len(out) >= _MAX_DT:
                return out
    if not out:
        out = [(d, 0) for d in range(min(state.D, _MAX_DT))]
    return out


class SearchStateV2(SearchState):
    """v0.1 incremental state (all mutation helpers inherited)."""

    def kmax(self) -> int:
        return self.max_segments - 1


# ---------------------------------------------------------------------------
# move families (each returns a move descriptor, never mutates the state)
# ---------------------------------------------------------------------------
def _best_twin(state: SearchStateV2, rng: random.Random):
    """Best budget-free improving twin ``(d, wps)`` over one-node edits."""
    spec, kmax = state.spec, state.kmax()
    v_cur = state.vector()
    best = None
    for d in _candidate_demands(state):
        base = state.wp[d][0]
        pool = _node_pool(state, rng, d)
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


def _best_single(state: SearchStateV2, rng: random.Random):
    """Best budget-respecting improving single-slot move ``(d, t, wps)``."""
    spec, kmax = state.spec, state.kmax()
    v_cur = state.vector()
    best = None
    for d, t in _candidate_slots(state):
        base = state.wp[d][t]
        pool = _node_pool(state, rng, d)
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


def _best_shift(state: SearchStateV2, rng: random.Random):
    """Coordinated ``(d_twin, twin_wps, d_spend, t_spend, wps)``.

    Twin a divergent demand (freeing its budget share) and spend that budget on
    a single-slot reroute of a different demand that single moves could not
    afford.  Budget check accounts for *both* demands' distance changes.
    """
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
            pool = _node_pool(state, rng, d_spend)
            for nw in _one_node_edits(base, pool, kmax):
                nd = state.distance_after_single(d_spend, t_spend, nw)
                # total distance after both moves
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
def local_search_v2(state: SearchStateV2, rng: random.Random,
                    max_seconds: float = 120.0) -> None:
    """Best-improvement descent over twin / single / shift until none improves."""
    t0 = time.time()
    while time.time() - t0 < max_seconds:
        mv = _best_twin(state, rng)
        if mv is not None:
            d, wps = mv
            state.apply_twin(d, wps)
            continue

        mv = _best_single(state, rng)
        if mv is not None:
            d, t, wps = mv
            state.apply_single(d, t, wps)
            continue

        mv = _best_shift(state, rng)
        if mv is not None:
            d_twin, twin_wps, d_spend, t_spend, wps = mv
            state.apply_twin(d_twin, twin_wps)
            state.apply_single(d_spend, t_spend, wps)
            continue

        break  # converged


def solve_with_time_budget_v2(
    inst: Instance,
    graph: DirectedGraph,
    cache: AtomCache,
    time_budget: float = 30.0,
    restarts: int = 3,
    seed: int = 0,
) -> Tuple[Solution, SearchStateV2, np.ndarray]:
    """Multi-start v0.2 local search; returns (best Solution, state, sat)."""
    from ..eval.evaluator import Evaluator

    rng = random.Random(seed)
    base_sat = Evaluator(inst, graph, cache).saturations(
        Solution.empty(inst.n_demands, inst.n_slots)).copy()
    best_state: Optional[SearchStateV2] = None
    best_vec: Optional[np.ndarray] = None
    per_restart = time_budget / max(restarts, 1)

    for r in range(restarts):
        state = SearchStateV2(inst, graph, cache)
        state.sat = base_sat.copy()
        state.recompute()
        local_search_v2(state, random.Random(seed + 997 * r),
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
