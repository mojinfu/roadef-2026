"""Read a ``*-srpaths.json`` solution file back into a :class:`Solution`.

The output file identifies waypoints by network-file node ids (see the writer);
here they are converted back to internal positions using the instance mapping.
"""
from __future__ import annotations

import json
from typing import Dict, Sequence

from ..model import Instance, Solution

__all__ = ["load_solution_from_dict", "read_solution"]


def load_solution_from_dict(doc: Dict, inst: Instance) -> Solution:
    """Build a rectangular :class:`Solution` from an srpaths JSON document.

    Entries may be omitted (== direct ECMP path, no waypoint).  Each present
    entry may use the keys ``d``/``t``/``w`` (matched by first letter, as the
    official checker does).
    """
    pos_of_id = inst.node_index
    sol = Solution.empty(inst.n_demands, inst.n_slots)
    for entry in doc.get("srpaths", []):
        d = t = None
        wps: Sequence[int] = ()
        for key, value in entry.items():
            if key[:1] == "d" and d is None:
                d = int(value)
            elif key[:1] == "t" and t is None:
                t = int(value)
            elif key[:1] == "w":
                wps = [int(x) for x in value]
        if d is None or t is None:
            raise ValueError(f"srpaths entry missing d/t: {entry}")
        if not (0 <= d < inst.n_demands):
            raise ValueError(f"demand id {d} out of range")
        if not (0 <= t < inst.n_slots):
            raise ValueError(f"time slot {t} out of range")
        try:
            positions = tuple(pos_of_id[w] for w in wps)
        except KeyError as exc:
            raise ValueError(f"waypoint {exc.args[0]} is not a node id of {inst.name}") from exc
        sol.set_waypoints(d, t, positions)
    return sol


def read_solution(path: str, inst: Instance) -> Solution:
    """Read a solution file, validating its structure."""
    with open(path, "r", encoding="utf-8") as fh:
        doc = json.load(fh)
    return load_solution_from_dict(doc, inst)
