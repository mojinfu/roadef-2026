"""Incremental local search for T-ASR.

State
-----
A candidate solution is an assignment ``wp[d][t]`` of waypoints to every
(demand, time-slot) pair (empty tuple == direct ECMP source->target).  The
search keeps the induced saturation matrix ``sat`` (arc x slot) up to date
incrementally so that evaluating a candidate move only costs the recomputed
flow vector of the touched demand plus a lexicographic comparison of the
sorted load vector.

Move neighbourhood
------------------
* ``single``: set ``wp[d][t]`` to a single-waypoint path (or to empty).
* ``twin``:   set ``wp[d][0] == wp[d][1]`` to the same path (free under the
  inter-slot budget, since identical paths cost distance 0).  Useful when the
  budget is tight.

All moves respect ``max_segments`` and, when a budget is defined for slot 1,
the inter-slot distance budget (Hamming distance over the segment-pair masks,
mirroring the official checker).

This is deliberately a *v0.1* engine: neighbourhood = single waypoints (+ a
small sampled set on large graphs).  It is a local search and will routinely
get stuck in second-order local optima; treat the numbers it produces as a
lower bound on what the codebase can reach.
"""
from __future__ import annotations

import random
import time
from typing import Optional, Sequence, Tuple

import numpy as np

from ..ecmp import AtomCache
from ..eval.evaluator import Evaluator
from ..eval.objective import CompareSpec, lex_compare_sorted, saturations_vector
from ..model import DirectedGraph, Instance, Solution

__all__ = ["SearchState", "local_search", "solve_with_time_budget"]


def _seg_pairs(wps: Tuple[int, ...], s: int, t: int):
    """Consecutive (u, v) node pairs of an SR path with the given waypoints."""
    seq = (s,) + tuple(wps) + (t,)
    out = []
    for u, w in zip(seq[:-1], seq[1:]):
        if u != w:
            out.append((u, w))
    return out


def path_distance(wps_a: Tuple[int, ...], s: int, t: int, wps_b: Tuple[int, ...]) -> int:
    """Symmetric-difference size of the segment-pair masks of two SR paths."""
    sa = set(_seg_pairs(wps_a, s, t))
    sb = set(_seg_pairs(wps_b, s, t))
    return len(sa ^ sb)


class SearchState:
    """Incremental state of a routing solution."""

    def __init__(self, inst: Instance, graph: DirectedGraph, cache: AtomCache):
        self.inst = inst
        self.g = graph
        self.cache = cache
        self.D = inst.n_demands
        self.H = inst.n_slots
        self.M = graph.m
        self.cap = np.array([a.capacity for a in graph.arcs], dtype=np.float64)
        self.spec = CompareSpec()
        self.budget1 = None  # set by caller from scenario budget[1]
        if 1 < inst.n_slots and inst.scenario.budget[1] is not None:
            self.budget1 = inst.scenario.budget[1]
        self.max_segments = inst.scenario.max_segments
        self.wp: list[list[tuple]] = [[() for _ in range(self.H)] for _ in range(self.D)]
        self.sat: np.ndarray = np.zeros((self.M, self.H))
        self._oldf: dict = {}  # (d, t) -> cached current flow vector
        self._dist: int = 0     # running inter-slot distance (maintained)
        self._ev = Evaluator(inst, graph, cache)

    # -- flow helpers ---------------------------------------------------
    def path_flow(self, d: int, t: int, wps: Tuple[int, ...]) -> Optional[np.ndarray]:
        """Total flow vector (arcs x 1) of demand d at slot t on path ``wps``."""
        dem = self.inst.demands[d]
        if len(wps) + 1 > self.max_segments:
            return None
        out = np.zeros(self.M)
        seq = (dem.source,) + tuple(wps) + (dem.target,)
        for u, w in zip(seq[:-1], seq[1:]):
            if u == w:
                continue
            atom = self.cache.atom(u, w, t)
            if atom is None:
                return None
            out += atom
        return out * dem.volume[t]

    def demand_flow(self, d: int, t: int) -> np.ndarray:
        """Current flow vector of (d, t), memoized."""
        key = (d, t)
        f = self._oldf.get(key)
        if f is None:
            f = self.path_flow(d, t, self.wp[d][t])
            if f is None:  # should not happen for a feasible state
                f = np.zeros(self.M)
            self._oldf[key] = f
        return f

    def recompute(self) -> np.ndarray:
        """Full recompute of saturations (used at startup / after warm starts)."""
        sat = np.zeros((self.M, self.H))
        for d in range(self.D):
            for t in range(self.H):
                f = self.path_flow(d, t, self.wp[d][t])
                if f is None:
                    continue
                sat[:, t] += f
        self.sat = sat / self.cap[:, None]
        self._oldf.clear()
        self._dist = 0
        for d in range(self.D):
            dem = self.inst.demands[d]
            self._dist += path_distance(self.wp[d][0], dem.source, dem.target, self.wp[d][1])
        return self.sat

    # -- distance / budget ---------------------------------------------
    def distance(self) -> int:
        return self._dist

    def _demand_dist(self, d: int) -> int:
        dem = self.inst.demands[d]
        return path_distance(self.wp[d][0], dem.source, dem.target, self.wp[d][1])

    def distance_after_single(self, d: int, t: int, wps: Tuple[int, ...]) -> int:
        if self.budget1 is None:
            return 0
        dem = self.inst.demands[d]
        other = self.wp[d][1 - t]
        return path_distance(wps, dem.source, dem.target, other)

    # -- move application ----------------------------------------------
    def apply_single(self, d: int, t: int, wps: Tuple[int, ...]) -> None:
        dem = self.inst.demands[d]
        old_dc = self._demand_dist(d)
        oldf = self.demand_flow(d, t)
        self.wp[d][t] = tuple(wps)
        newf = self.path_flow(d, t, self.wp[d][t])
        if newf is None:
            self.wp[d][t] = ()
            newf = self.demand_flow(d, t)
        self.sat[:, t] += (newf - oldf) / self.cap
        self._oldf[(d, t)] = newf
        self._dist += self._demand_dist(d) - old_dc

    def apply_twin(self, d: int, wps: Tuple[int, ...]) -> None:
        dem = self.inst.demands[d]
        old_dc = self._demand_dist(d)
        f0 = self.path_flow(d, 0, wps)
        f1 = self.path_flow(d, 1, wps)
        old0 = self.demand_flow(d, 0)
        old1 = self.demand_flow(d, 1)
        self.wp[d][0] = tuple(wps)
        self.wp[d][1] = tuple(wps)
        self.sat[:, 0] += (f0 - old0) / self.cap
        self.sat[:, 1] += (f1 - old1) / self.cap
        self._oldf[(d, 0)] = f0
        self._oldf[(d, 1)] = f1
        self._dist += 0 - old_dc

    def vector(self) -> np.ndarray:
        return saturations_vector(self.sat)


def _candidate_waypoints(state: SearchState, rng: random.Random, d: int, t: int) -> list:
    """Nodes usable as a single waypoint for demand d at slot t."""
    dem = state.inst.demands[d]
    nodes = [w for w in range(state.g.n_nodes) if w not in (dem.source, dem.target)]
    if len(nodes) <= 120:
        return nodes
    # Large graphs: a random sample is all we can afford per iteration.
    rng.shuffle(nodes)
    return nodes[:64]


def local_search(
    state: SearchState,
    rng: random.Random,
    max_iter: int = 500,
    max_seconds: float = 120.0,
    twin_when_budget_tight: bool = True,
) -> None:
    """Iterated best-improvement over single-waypoint moves (in place).

    Returns when no improving move is found for the current hot-arc set or the
    iteration / time budget is exhausted.
    """
    inst = state.inst
    t0 = time.time()

    for _ in range(max_iter):
        if time.time() - t0 > max_seconds:
            break
        v_cur = state.vector()
        best_v = None
        best_move = None

        # Hot arcs: arcs in the top of the load profile.  Moves that do not
        # touch any of them cannot improve the top of the vector.
        order = np.argsort(-state.sat, axis=None)
        m, h = state.sat.shape
        n_hot = min(12, m * h)
        hot = [(int(idx // h), int(idx % h)) for idx in order[:n_hot]]
        # Also chase anything within 0.5% of the top load.
        top_val = state.sat.flat[order[0]]
        for idx in order:
            a, t = int(idx // h), int(idx % h)
            if state.sat[a, t] >= top_val * 0.995:
                hot.append((a, t))
            else:
                break

        # Demands currently flowing through hot arcs, sorted by contribution.
        cand_dt = set()
        contrib = []
        for a, t in hot:
            for d in range(state.D):
                f = state.demand_flow(d, t)
                c = f[a]
                if c > 1e-9:
                    contrib.append((c, d, t, a))
        contrib.sort(reverse=True)
        # Cap the batch of (d,t) pairs we examine each iteration.
        seen = set()
        for c, d, t, a in contrib:
            if (d, t) in seen:
                continue
            seen.add((d, t))
            cand_dt.add((d, t, a))
            if len(cand_dt) >= 60:
                break

        for d, t, hot_arc in sorted(cand_dt):
            dem = inst.demands[d]
            for w in _candidate_waypoints(state, rng, d, t):
                wps = (w,)
                newd = state.distance_after_single(d, t, wps)
                if state.budget1 is not None and state.distance() - state._demand_dist(d) + newd > state.budget1:
                    continue
                newf = state.path_flow(d, t, wps)
                if newf is None:
                    continue
                ns = state.sat.copy()
                ns[:, t] += (newf - state.demand_flow(d, t)) / state.cap
                vn = saturations_vector(ns)
                if lex_compare_sorted(vn, v_cur, state.spec.decimals) == -1:
                    if best_v is None or lex_compare_sorted(vn, best_v, state.spec.decimals) == -1:
                        best_v = vn
                        best_move = (d, t, wps)
            # Empty path (revert to direct) is always a candidate.
            wps = ()
            newd = state.distance_after_single(d, t, wps)
            if state.budget1 is None or state.distance() - state._demand_dist(d) + newd <= state.budget1:
                newf = state.path_flow(d, t, wps)
                if newf is not None:
                    ns = state.sat.copy()
                    ns[:, t] += (newf - state.demand_flow(d, t)) / state.cap
                    vn = saturations_vector(ns)
                    if lex_compare_sorted(vn, v_cur, state.spec.decimals) == -1:
                        if best_v is None or lex_compare_sorted(vn, best_v, state.spec.decimals) == -1:
                            best_v = vn
                            best_move = (d, t, wps)

        if best_move is None:
            # Try twin moves (identical path on both slots) when stuck and
            # budget is the limiting factor.
            if twin_when_budget_tight:
                twin_v = None
                twin_move = None
                for d, t, hot_arc in sorted(cand_dt):
                    dem = inst.demands[d]
                    for w in _candidate_waypoints(state, rng, d, t):
                        wps = (w,)
                        # Twin: identical on both slots -> distance 0 for d.
                        f0 = state.path_flow(d, 0, wps)
                        f1 = state.path_flow(d, 1, wps)
                        if f0 is None or f1 is None:
                            continue
                        ns = state.sat.copy()
                        ns[:, 0] += (f0 - state.demand_flow(d, 0)) / state.cap
                        ns[:, 1] += (f1 - state.demand_flow(d, 1)) / state.cap
                        vn = saturations_vector(ns)
                        if lex_compare_sorted(vn, state.vector(), state.spec.decimals) == -1:
                            if twin_v is None or lex_compare_sorted(vn, twin_v, state.spec.decimals) == -1:
                                twin_v = vn
                                twin_move = (d, wps)
                if twin_move is not None:
                    d, wps = twin_move
                    state.apply_twin(d, wps)
                    continue
            break

        d, t, wps = best_move
        state.apply_single(d, t, wps)


def solve_with_time_budget(
    inst: Instance,
    graph: DirectedGraph,
    cache: AtomCache,
    time_budget: float = 30.0,
    restarts: int = 3,
    seed: int = 0,
) -> Tuple[Solution, SearchState, np.ndarray]:
    """Multi-start local search; returns the best (Solution, state, sat) found."""
    rng = random.Random(seed)
    # One-time baseline saturation (empty waypoints) reused by every restart.
    base_sat = Evaluator(inst, graph, cache).saturations(Solution.empty(inst.n_demands, inst.n_slots)).copy()
    best_state = None
    best_vec = None

    for r in range(restarts):
        state = SearchState(inst, graph, cache)
        state.sat = base_sat.copy()
        # seed per restart
        rs = random.Random(seed + 997 * r)
        local_search(state, rs, max_iter=10_000, max_seconds=time_budget / max(restarts, 1))
        v = state.vector()
        if best_vec is None or lex_compare_sorted(v, best_vec, state.spec.decimals) == -1:
            best_state = state
            best_vec = v

    sol = Solution.empty(inst.n_demands, inst.n_slots)
    for d in range(inst.n_demands):
        for t in range(inst.n_slots):
            sol.set_waypoints(d, t, best_state.wp[d][t])
    return sol, best_state, best_state.sat
