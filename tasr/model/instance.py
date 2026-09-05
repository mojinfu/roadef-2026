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
    """A complete T-ASR instance (network + demands + scenario)."""

    name: str
    node_names: Tuple[str, ...]      # node id -> name
    arcs: Tuple[Arc, ...]            # arc id == index
    n_slots: int
    demands: Tuple[Demand, ...]      # demand id == index (position in the JSON array)
    scenario: Scenario

    # ---- convenience -------------------------------------------------------
    @property
    def n_nodes(self) -> int:
        return len(self.node_names)

    @property
    def n_arcs(self) -> int:
        return len(self.arcs)

    @property
    def n_demands(self) -> int:
        return len(self.demands)

    def demand_at(self, slot: int, demand_id: int) -> float:
        return self.demands[demand_id].volume[slot]
