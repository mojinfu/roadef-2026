"""Representation of a routing scheme (the solver's decision object)."""
from __future__ import annotations

from dataclasses import dataclass, field
from typing import List, Tuple


@dataclass
class Solution:
    """Segment path chosen for every (demand, time slot).

    ``waypoints[d][t]`` is a tuple of waypoint node ids (excluding source and
    target).  An empty tuple means: plain shortest-path routing (no segment).

    The array is always rectangular: every demand has exactly ``n_slots``
    entries, even when the corresponding traffic volume is zero.  This makes
    budget computation and delta evaluation uniform.
    """

    n_demands: int
    n_slots: int
    waypoints: List[List[Tuple[int, ...]]] = field(default_factory=list)

    @staticmethod
    def empty(n_demands: int, n_slots: int) -> "Solution":
        """Trivial solution: no waypoint at all, for every demand and slot."""
        return Solution(
            n_demands=n_demands,
            n_slots=n_slots,
            waypoints=[[() for _ in range(n_slots)] for _ in range(n_demands)],
        )

    def copy(self) -> "Solution":
        return Solution(
            n_demands=self.n_demands,
            n_slots=self.n_slots,
            waypoints=[list(row) for row in self.waypoints],
        )

    def set_waypoints(self, demand: int, slot: int, wps: Tuple[int, ...]) -> None:
        self.waypoints[demand][slot] = tuple(wps)

    def __eq__(self, other) -> bool:
        return (
            isinstance(other, Solution)
            and self.n_demands == other.n_demands
            and self.n_slots == other.n_slots
            and self.waypoints == other.waypoints
        )
