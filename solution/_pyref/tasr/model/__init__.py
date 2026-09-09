"""Pure data model for T-ASR instances and solutions (no solver dependency)."""
from .instance import Arc, Demand, Instance, Scenario
from .graph import DirectedGraph
from .solution import Solution

__all__ = [
    "Arc",
    "Demand",
    "Instance",
    "Scenario",
    "DirectedGraph",
    "Solution",
]
