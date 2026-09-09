"""Immutable representation of a T-ASR instance.

Everything here is plain data.  Parsing (JSON) lives in :mod:`tasr.io.parser`,
graph algorithms in :mod:`tasr.model.graph`.
"""
from __future__ import annotations

from dataclasses import dataclass
from typing import Tuple


@dataclass(frozen=True)
class Arc:
    """A directed arc (link) of the network.  ``id`` equals its index in the arc list."""

    id: int
    frm: int
    to: int
    metric: float
    capacity: float

    @property
    def reverse_key(self) -> Tuple[int, int]:
        return (self.to, self.frm)


@dataclass(frozen=True)
class Demand:
    """A (source, target) couple with one traffic volume per time slot."""

    source: int
    target: int
    volume: Tuple[float, ...]  # length == number of time slots


@dataclass(frozen=True)
class Scenario:
    """Intervention scenario + per-step reconfiguration budget."""

    max_segments: int
    budget: Tuple[int, ...]          # length == n_slots ; budget[0] == 0 (no constraint)
    interventions: Tuple[frozenset, ...]  # per time slot: set of down arc ids


@dataclass(frozen=True)
class Instance:
    """A complete T-ASR instance (network + demands + scenario).

    Node identity: the network / traffic files identify nodes by an integer
    ``id`` that may be listed in *any* order (setA-05/08/12/14/17 are reversed).
    Internally we use contiguous *positions* ``0..n_nodes-1``; ``node_ids`` maps
    a position back to its file ``id`` and demands/waypoints/arc endpoints are
    stored as positions.  Convert at the I/O boundary (writer emits ids).
    """

    name: str
    node_names: Tuple[str, ...]      # position -> display name
    node_ids: Tuple[int, ...]        # position -> network-file node id
    arcs: Tuple[Arc, ...]            # arc id == index
    n_slots: int
    demands: Tuple[Demand, ...]      # demand id == index (position in the JSON array)
    scenario: Scenario

    # ---- convenience -------------------------------------------------------
    @property
    def n_nodes(self) -> int:
        return len(self.node_names)

    @property
    def node_index(self) -> dict:
        """network-file id -> internal position."""
        return {nid: i for i, nid in enumerate(self.node_ids)}

    @property
    def n_arcs(self) -> int:
        return len(self.arcs)

    @property
    def n_demands(self) -> int:
        return len(self.demands)

    def demand_at(self, slot: int, demand_id: int) -> float:
        return self.demands[demand_id].volume[slot]
