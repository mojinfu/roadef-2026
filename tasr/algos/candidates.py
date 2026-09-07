"""Distance-derived waypoint candidate generation (solver v0.3).

The uniform-random node pool of v0.2 spends most of its evaluations on nodes far
from a demand's actual corridors.  The shortest-path data the ECMP computation
already builds (forward/backward distance arrays over the slot graph) tells us
exactly which nodes lie on -- or cheaply detour around -- a demand's segment
legs.  :class:`~tasr.ecmp.atoms.compute_atom` discards that data after summing
the flow into one atom vector; this module keeps it and uses it to propose
candidate waypoint nodes.

Geometry
--------
For a leg ``u -> v`` in the slot-``t`` graph (down arcs removed), inserting a
waypoint ``C`` routes the leg as ``u -> C`` + ``C -> v`` with metric overhead

    slack(C) = dist(u, C) + dist(C, v) - dist(u, v).          (*)

* ``slack ~ 0`` nodes sit inside the shortest-path DAG of ``u -> v``: the new
  legs only re-weight the ECMP split *within* the current corridor (a useful
  tool for tail balancing / concentrating flow on an under-loaded branch).
* small positive ``slack`` nodes are cheap detours that bring *new* arcs into
  the path -- the only way out when the hot arc is on every shortest path.

The candidate set is the corridor of each hot-carrying leg, ordered by relative
slack ``slack / dist(u, v)`` so cheap detours are proposed first.  Distances
depend only on the (static) slot graph, so :class:`DistanceIndex` memoizes one
forward and one backward Dijkstra per (node, slot) across the whole search.
"""
from __future__ import annotations

from typing import List, Optional, Sequence, Tuple

import numpy as np

from ..ecmp import distances_from, distances_to
from ..model import DirectedGraph

__all__ = ["DistanceIndex", "leg_corridor"]

_INF = float("inf")


class DistanceIndex:
    """Memoized per-(node, slot) shortest distances over each slot's graph.

    The IGP metrics and the intervention (down) arcs are fixed for a solve, so
    these arrays never change while the waypoints do; caching them once lets a
    candidate generator test any node ``C`` against a leg in O(1) arithmetic.
    """

    def __init__(self, graph: DirectedGraph, interventions: Sequence) -> None:
        self.g = graph
        self._blocked = tuple(frozenset(x) for x in interventions)
        self._fwd: dict = {}  # (u, t) -> dist from u
        self._rev: dict = {}  # (v, t) -> dist to v

    def fwd(self, u: int, t: int) -> np.ndarray:
        """``dist[x]`` = shortest ``u -> x`` in the slot-``t`` graph."""
        key = (u, t)
        arr = self._fwd.get(key)
        if arr is None:
            arr = distances_from(self.g, u, self._blocked[t])
            self._fwd[key] = arr
        return arr

    def rev(self, v: int, t: int) -> np.ndarray:
        """``dist[x]`` = shortest ``x -> v`` in the slot-``t`` graph."""
        key = (v, t)
        arr = self._rev.get(key)
        if arr is None:
            arr = distances_to(self.g, v, self._blocked[t])
            self._rev[key] = arr
        return arr


def leg_corridor(
    idx: DistanceIndex,
    u: int,
    v: int,
    t: int,
    rel_detour: float,
    exclude: Optional[set] = None,
) -> List[Tuple[float, int]]:
    """Waypoint nodes ``C`` that split leg ``u -> v`` inside a cheap corridor.

    Returns ``[(rel_slack, node), ...]`` sorted ascending (cheapest detour
    first).  ``rel_slack = slack / dist(u,v)`` with the slack of (*).  ``u``,
    ``v`` themselves and nodes in ``exclude`` are skipped; nodes for which either
    distance is infinite (unreachable once the down arcs are removed) are too.
    Empty if ``u`` and ``v`` are disconnected at slot ``t``.
    """
    g = idx.g
    du = idx.fwd(u, t)
    dv = idx.rev(v, t)
    duv = du[v]
    if not np.isfinite(duv) or duv <= 0.0:
        return []
    tol = max(1e-9, duv * 1e-9)
    cap = rel_detour * duv + tol
    out: List[Tuple[float, int]] = []
    for C in range(g.n_nodes):
        if C == u or C == v or (exclude is not None and C in exclude):
            continue
        a = du[C]
        b = dv[C]
        if not np.isfinite(a) or not np.isfinite(b):
            continue
        slack = a + b - duv
        if slack < -tol or slack > cap:
            continue
        out.append((slack / duv, C))
    out.sort(key=lambda x: x[0])
    return out
