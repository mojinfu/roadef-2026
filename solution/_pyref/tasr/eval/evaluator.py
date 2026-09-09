"""Fast load evaluation through superposition of precomputed ECMP atoms.

A routing scheme assigns each (demand, slot) a segment path = an ordered list
of waypoints.  Between two consecutive points ``(u, v)`` the traffic follows
``FG_t(u, v)`` under ECMP, whose split coefficients are precomputed once by
:class:`tasr.ecmp.AtomCache`.  Because the forwarding of one demand does not
depend on the others, the load of an arc is a *linear* sum of per-segment
atom contributions -- this is what makes delta re-evaluation cheap later.
"""
from __future__ import annotations

import numpy as np

from ..ecmp import AtomCache
from ..model import DirectedGraph, Instance, Solution

__all__ = ["Evaluator"]


class RoutingInfeasibleError(RuntimeError):
    """Raised when a segment (u, v) is not connected in the network at slot t."""


class Evaluator:
    """Computes arc saturation matrices for :class:`Solution` objects."""

    def __init__(self, inst: Instance, graph: DirectedGraph, cache: AtomCache):
        self.inst = inst
        self.graph = graph
        self.cache = cache
        self.capacity = np.array([a.capacity for a in graph.arcs], dtype=np.float64)

    # ------------------------------------------------------------------
    def segments_of(self, demand_id: int, slot: int, waypoints):
        """Consecutive (u, v) node pairs of the segment path, empty segments skipped."""
        dem = self.inst.demands[demand_id]
        pts = [dem.source, *waypoints, dem.target]
        return [(pts[i], pts[i + 1]) for i in range(len(pts) - 1) if pts[i] != pts[i + 1]]

    def demand_atom(self, demand_id: int, slot: int, waypoints) -> np.ndarray:
        """Unit-flow load contribution (arc vector) of one (d, t) routing."""
        acc = None
        for (u, v) in self.segments_of(demand_id, slot, waypoints):
            atom = self.cache.atom(u, v, slot)
            if atom is None:
                raise RoutingInfeasibleError(
                    f"demand {demand_id} segment {u}->{v} disconnected at slot {slot}"
                )
            acc = atom if acc is None else acc + atom
        if acc is None:
            acc = np.zeros(self.graph.m, dtype=np.float64)
        return acc

    # ------------------------------------------------------------------
    def saturations(self, sol: Solution) -> np.ndarray:
        """Saturation matrix ``shape = (n_arcs, n_slots)`` = flow / capacity.

        Zero-volume (d, t) pairs are skipped (they contribute nothing).
        """
        m = self.graph.m
        n_slots = self.inst.n_slots
        flows = np.zeros((m, n_slots), dtype=np.float64)
        cache = self.cache

        for d, dem in enumerate(self.inst.demands):
            for t in range(n_slots):
                vol = dem.volume[t]
                if vol == 0.0:
                    continue
                wps = sol.waypoints[d][t]
                acc = np.zeros(m, dtype=np.float64)
                for (u, v) in self.segments_of(d, t, wps):
                    atom = cache.atom(u, v, t)
                    if atom is None:
                        raise RoutingInfeasibleError(
                            f"demand {d} segment {u}->{v} disconnected at slot {t}"
                        )
                    acc += atom
                flows[:, t] += vol * acc

        cap = self.capacity[:, None]
        return flows / cap

    def max_saturation(self, sol: Solution) -> float:
        return float(np.max(self.saturations(sol)))

    def flow_matrix(self, sol: Solution) -> np.ndarray:
        """Return the raw flow matrix (arcs x slots) without capacity division."""
        return self.saturations(sol) * self.capacity[:, None]
