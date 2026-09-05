"""Lock the comparison semantics: loads truncated to 6 decimals (no rounding)."""
from __future__ import annotations

import numpy as np

from tasr.eval import CompareSpec, lex_compare_sorted, saturations_vector, truncate_loads


def test_truncate_drops_from_7th_digit():
    x = np.array([0.123456789, 1.2345678, 0.9999999999, 0.5])
    got = truncate_loads(x)
    assert got[0] == 0.123456
    assert got[1] == 1.234567  # NOT 1.234568 (no rounding)
    assert got[2] == 0.999999  # NOT 1.0
    assert got[3] == 0.5


def test_truncate_is_not_round():
    # 0.9999999 truncates to 0.999999 while round would give 1.000000
    assert truncate_loads(np.array([0.9999999]))[0] == 0.999999
    assert CompareSpec(mode="round").quantize(np.array([0.9999999]))[0] == 1.0


def test_lex_compare_first_diff_after_truncation():
    # identical under truncation -> tie even if full precision differs slightly
    a = np.array([0.5, 0.3, 0.1])
    b = np.array([0.5, 0.3000004, 0.1])  # 0.3000004 trunc -> 0.300000 == 0.3
    assert lex_compare_sorted(a, b) == 0

    # strict improvement only visible at the 6th decimal boundary
    c = np.array([0.5, 0.299999, 0.1])  # 0.299999 < 0.300000 at the same position
    assert lex_compare_sorted(a, c) == 1  # c strictly better (smaller)
    assert lex_compare_sorted(c, a) == -1


def test_sorted_desc_vector():
    sat = np.array([[0.1, 0.9], [0.5, 0.5]])
    v = saturations_vector(sat)
    assert list(v) == [0.9, 0.5, 0.5, 0.1]
