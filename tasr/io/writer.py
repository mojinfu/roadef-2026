"""Write a :class:`~tasr.model.Solution` to the ``*-srpaths.json`` output format.

Every (demand, time slot) pair is written explicitly -- even with an empty
waypoint list ``w: []`` -- so the checker never has to guess a default for an
omitted entry.  This removes the ambiguity between an omitted (d,t) and an
explicit empty segment path.
"""
from __future__ import annotations

import json
from typing import Dict, List

from ..model import Instance, Solution

__all__ = ["solution_to_dict", "write_solution"]


def solution_to_dict(sol: Solution, inst: Instance) -> Dict:
    """Serialize a solution to the official JSON structure.

    Entries are sorted by demand id, then time slot, for reproducibility.
    """
    out: List[Dict] = []
    for d in range(sol.n_demands):
        for t in range(sol.n_slots):
            wps = sol.waypoints[d][t]
            if wps:
                # validate node ids exist (defensive; waypoints are node ids)
                for w in wps:
                    if not (0 <= w < inst.n_nodes):
                        raise ValueError(f"waypoint {w} out of range (demand {d}, t={t})")
                w_list = [int(w) for w in wps]
            else:
                w_list = []
            out.append({"d": d, "t": t, "w": w_list})
    return {"srpaths": out}


def write_solution(path: str, sol: Solution, inst: Instance) -> None:
    """Write the solution JSON to ``path``."""
    data = solution_to_dict(sol, inst)
    with open(path, "w", encoding="utf-8") as fh:
        json.dump(data, fh, ensure_ascii=False, separators=(",", ":"))
