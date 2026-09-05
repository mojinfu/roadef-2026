"""Simple construction heuristics used as a starting point."""
from __future__ import annotations

from ..model import Instance, Solution

__all__ = ["baseline_solution"]


def baseline_solution(inst: Instance) -> Solution:
    """Trivial solution: shortest-path routing (no waypoints) everywhere.

    It never violates the budget (nothing changes between slots) and is the
    natural reference point for any improvement phase.
    """
    return Solution.empty(inst.n_demands, inst.n_slots)
