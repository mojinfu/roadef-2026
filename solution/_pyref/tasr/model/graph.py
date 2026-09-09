"""Adjacency structure built from an :class:`Instance` for graph algorithms."""
from __future__ import annotations

from typing import List, Sequence, Tuple

from .instance import Arc, Instance


class DirectedGraph:
    """Lightweight adjacency container.

    Nodes are 0..n_nodes-1, arcs are 0..m-1 and an arc's id equals its index.
    Both forward (``outs``) and backward (``ins``) adjacency are kept so that
    Dijkstra can run in either direction (needed for ECMP forwarding graphs).
    """

    __slots__ = ("n_nodes", "node_names", "arcs", "m", "outs", "ins")

    def __init__(self, node_names: Sequence[str], arcs: Sequence[Arc]):
        self.n_nodes = len(node_names)
        self.node_names = tuple(node_names)
        # Guard: arc ids must be 0..m-1 in order.
        arcs = tuple(arcs)
        for i, a in enumerate(arcs):
            if a.id != i:
                raise ValueError(f"arc id {a.id} != its position {i}; arcs must be contiguous")
        self.arcs = arcs
        self.m = len(arcs)

        outs: List[List[Arc]] = [[] for _ in range(self.n_nodes)]
        ins: List[List[Arc]] = [[] for _ in range(self.n_nodes)]
        for a in arcs:
            if a.frm < 0 or a.frm >= self.n_nodes or a.to < 0 or a.to >= self.n_nodes:
                raise ValueError(f"arc {a.id} endpoints out of range: {a.frm}->{a.to}")
            outs[a.frm].append(a)
            ins[a.to].append(a)
        self.outs = tuple(tuple(x) for x in outs)  # type: Tuple[Tuple[Arc, ...], ...]
        self.ins = tuple(tuple(x) for x in ins)  # type: Tuple[Tuple[Arc, ...], ...]

    # ---- helpers -----------------------------------------------------------
    def arc(self, arc_id: int) -> Arc:
        return self.arcs[arc_id]

    @staticmethod
    def build(inst: Instance) -> "DirectedGraph":
        return DirectedGraph(inst.node_names, inst.arcs)
