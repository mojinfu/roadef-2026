"""Split coefficients ``r(u, v, a, t)`` under IGP ECMP.

Definition (challenge subject, §2):
  * The forwarding graph ``FG(u, v)`` in ``G_t`` is the subgraph of arcs that
    belong to *some* shortest path from ``u`` to ``v`` according to arc
    metrics, where arcs down by an intervention at time ``t`` are removed.
  * ECMP: the flow entering a vertex of ``FG(u, v)`` is *evenly divided*
    between the outgoing arcs of that vertex inside ``FG(u, v)``.

``compute_atom`` returns, for a unit demand from ``u`` to ``v``, the fraction
of flow traversing each arc -- i.e. exactly the vector
``r(u, v, *, t)``.  The evaluator then superposes these per-demand vectors
weighted by traffic volume.
"""
from __future__ import annotations

import heapq
import math
from collections import OrderedDict
from typing import FrozenSet, Iterable, Optional, Sequence, Tuple

import numpy as np

from ..model import DirectedGraph

__all__ = ["compute_atom", "AtomCache"]

_INF = float("inf")
# Metrics are arbitrary floats; shortest-path distances are sums of many such
# values, so float round-off makes an exact ``== d`` equality unreliable.
# Use a tolerance relative to the shortest-path length.
_TOL = 1e-9


def _dijkstra(
    graph: DirectedGraph, source: int, blocked: FrozenSet[int], reverse: bool
) -> np.ndarray:
    """Shortest distances, skipping arcs whose id is in ``blocked``.

    ``reverse=False`` -> ``dist[x]`` = length of shortest ``source -> x`` path.
    ``reverse=True``  -> ``dist[x]`` = length of shortest ``x -> source`` path
    (Dijkstra run on the reversed graph, i.e. from the target).
    """
    dist = np.full(graph.n_nodes, _INF, dtype=np.float64)
    dist[source] = 0.0
    heap = [(0.0, source)]
    adjacency = graph.ins if reverse else graph.outs

    while heap:
        d, node = heapq.heappop(heap)
        if d > dist[node]:
            continue
        for arc in adjacency[node]:
            if arc.id in blocked:
                continue
            nxt = arc.frm if reverse else arc.to
            nd = d + arc.metric
            if nd < dist[nxt]:
                dist[nxt] = nd
                heapq.heappush(heap, (nd, nxt))
    return dist


def _tight_arcs(
    graph: DirectedGraph,
    node: int,
    dist_f: np.ndarray,
    dist_r: np.ndarray,
    d: float,
    blocked: FrozenSet[int],
):
    """Outgoing arcs of ``node`` belonging to FG(u,v) (i.e. on a shortest path)."""
    out = []
    for arc in graph.outs[node]:
        if arc.id in blocked:
            continue
        # arc (node -> arc.to) lies on a shortest u->v path iff
        #   dist_f[node] + metric + dist_r[arc.to] == dist_f[v] == d
        lhs = dist_f[node] + arc.metric + dist_r[arc.to]
        if abs(lhs - d) <= _TOL * (1.0 + abs(d)):
            out.append(arc)
    return out


def compute_atom(
    graph: DirectedGraph,
    u: int,
    v: int,
    blocked: Iterable[int] = (),
) -> Optional[np.ndarray]:
    """Unit-flow split coefficients ``r(u, v, *, t)``.

    Returns a float64 array of length ``graph.m`` with the fraction of a unit
    demand crossing each arc, or ``None`` if ``u`` and ``v`` are disconnected
    once ``blocked`` arcs are removed.
    """
    if u == v:
        return np.zeros(graph.m, dtype=np.float64)

    blocked = frozenset(blocked)
    dist_f = _dijkstra(graph, u, blocked, reverse=False)
    dist_r = _dijkstra(graph, v, blocked, reverse=True)

    d_uv = dist_f[v]
    if math.isinf(d_uv):
        return None

    atom = np.zeros(graph.m, dtype=np.float64)
    flow = {u: 1.0}

    # Process nodes in increasing distance-from-source order.  Every tight arc
    # strictly increases dist_f (metrics are positive), so flow only moves
    # forward along the ordering.
    order = [node for node in range(graph.n_nodes) if math.isfinite(dist_f[node])]
    order.sort(key=lambda x: dist_f[x])

    for node in order:
        f_node = flow.get(node, 0.0)
        if f_node == 0.0:
            continue
        tight = _tight_arcs(graph, node, dist_f, dist_r, d_uv, blocked)
        if not tight:
            if node != v:
                # Flow must never stall on a non-target vertex of FG.
                raise RuntimeError(f"ECMP flow stalled at node {node} (u={u}, v={v})")
            continue
        share = f_node / len(tight)
        for arc in tight:
            atom[arc.id] += share
            flow[arc.to] = flow.get(arc.to, 0.0) + share

    return atom


class AtomCache:
    """LRU cache of ``atom(u, v, t)`` vectors.

    Time slot ``t`` carries its own set of down arcs (interventions).  Vectors
    are cached as raw float64 bytes and decoded on demand; they are immutable.
    """

    def __init__(
        self,
        graph: DirectedGraph,
        interventions: Sequence[FrozenSet[int]],
        maxsize: int = 20000,
    ):
        self.graph = graph
        self.n_slots = len(interventions)
        self._interventions = tuple(frozenset(x) for x in interventions)
        self._maxsize = maxsize
        self._cache: "OrderedDict[Tuple[int, int, int], Optional[bytes]]" = OrderedDict()

    def _load(self, u: int, v: int, t: int) -> Optional[np.ndarray]:
        atom = compute_atom(self.graph, u, v, self._interventions[t])
        return None if atom is None else atom

    def atom(self, u: int, v: int, t: int) -> Optional[np.ndarray]:
        """Split-coefficient vector for segment ``(u, v)`` at slot ``t``."""
        if not (0 <= t < self.n_slots):
            raise IndexError(f"slot {t} out of range [0,{self.n_slots})")

        key = (u, v, t)
        raw = self._cache.get(key, _MISSING)
        if raw is _MISSING:
            atom = self._load(u, v, t)
            raw = None if atom is None else atom.tobytes()
            self._cache[key] = raw
            if len(self._cache) > self._maxsize:
                self._cache.popitem(last=False)
        if raw is None:
            return None
        return np.frombuffer(raw, dtype=np.float64)

    def clear(self) -> None:
        self._cache.clear()

    def __len__(self) -> int:
        return len(self._cache)


class _Missing:
    def __repr__(self) -> str:
        return "<missing>"


_MISSING = _Missing()
