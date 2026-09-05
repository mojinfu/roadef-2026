"""Lexicographic objective over the vector of arc loads.

The challenge ranks solutions by comparing the **sorted (decreasing) vector of
arc loads across all arcs and all time slots**; a solution is strictly better
iff that vector is lexicographically smaller.

Comparison precision (confirmed with the organizer's checker behaviour): every
load value is first *truncated* to 6 decimal places (digits from the 7th on
are dropped, not rounded).  Two loads that coincide after truncation tie.

Implementation detail: instead of comparing truncated floats we compare the
underlying integers ``q_i = floor(x_i * 10^decimals)``, which is exact.
"""
from __future__ import annotations

from dataclasses import dataclass
from typing import Optional

import numpy as np

__all__ = [
    "CompareSpec",
    "truncate_loads",
    "saturations_vector",
    "lex_compare_sorted",
    "compare_saturation_matrices",
]

# Small epsilon (in scaled units, i.e. already multiplied by 10^decimals) that
# absorbs floating-point representation noise when a value sits a hair below a
# decimal boundary but is mathematically exactly on it.
_SCALED_EPS = 1e-6


@dataclass(frozen=True)
class CompareSpec:
    """How loads are quantized before lexicographic comparison."""

    decimals: int = 6
    mode: str = "trunc"  # "trunc" (drop from 7th digit) or "round"

    @property
    def scale(self) -> int:
        return 10 ** self.decimals

    def quantize(self, x: np.ndarray) -> np.ndarray:
        """Return loads truncated/rounded to ``decimals`` places (float grid)."""
        s = x * self.scale
        if self.mode == "trunc":
            s = np.floor(s + _SCALED_EPS)
        elif self.mode == "round":
            s = np.floor(s + 0.5)
        else:
            raise ValueError(f"unknown quantization mode {self.mode!r}")
        return s / self.scale

    def rank_int(self, x: np.ndarray) -> np.ndarray:
        """Quantized loads as exact integers ``floor(x * 10^decimals)``."""
        s = x * self.scale
        if self.mode == "trunc":
            s = np.floor(s + _SCALED_EPS)
        elif self.mode == "round":
            s = np.floor(s + 0.5)
        else:
            raise ValueError(f"unknown quantization mode {self.mode!r}")
        return s.astype(np.int64)


def truncate_loads(x: np.ndarray, decimals: int = 6, mode: str = "trunc") -> np.ndarray:
    """Truncate/round a load array to ``decimals`` decimal places."""
    return CompareSpec(decimals=decimals, mode=mode).quantize(x)


def saturations_vector(sat: np.ndarray) -> np.ndarray:
    """Flatten a (arcs x slots) saturation matrix into one sorted-descending vector."""
    return np.sort(sat.ravel())[::-1]


def lex_compare_sorted(
    a_desc: np.ndarray,
    b_desc: np.ndarray,
    decimals: int = 6,
    mode: str = "trunc",
) -> int:
    """Lexicographic compare of two sorted-descending load vectors.

    Returns -1 if ``a`` is strictly better (smaller), +1 if ``b`` is, 0 on a tie.
    """
    spec = CompareSpec(decimals=decimals, mode=mode)
    ia = spec.rank_int(a_desc)
    ib = spec.rank_int(b_desc)
    return _compare_int_arrays(ia, ib)


def _compare_int_arrays(ia: np.ndarray, ib: np.ndarray) -> int:
    if len(ia) != len(ib):
        raise ValueError(f"vectors of different lengths: {len(ia)} vs {len(ib)}")
    d = ia - ib
    idx = np.flatnonzero(d)
    if idx.size == 0:
        return 0
    i = int(idx[0])
    return -1 if d[i] < 0 else 1


def compare_saturation_matrices(
    sat_a: np.ndarray,
    sat_b: np.ndarray,
    spec: Optional[CompareSpec] = None,
) -> int:
    """Convenience: compare two (arcs x slots) saturation matrices."""
    spec = spec or CompareSpec()
    va = saturations_vector(sat_a)
    vb = saturations_vector(sat_b)
    return lex_compare_sorted(va, vb, decimals=spec.decimals, mode=spec.mode)
