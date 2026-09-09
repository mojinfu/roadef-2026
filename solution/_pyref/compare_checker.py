"""Compare the official checker's saturation vector against the Go/Python
trunc6 rank vector.

Usage:
    python compare_checker.py <checker.json> <oracle-or-go.txt>

The checker JSON is its stdout (valid solution): read the "saturations" array,
which is the global (arcs x slots) saturation list sorted descending.  The text
file is oracle_sat.py / validate -dump output; we use its V/Q records.
"""
import json
import sys


def rank(x):
    return int(x * 1e6 + 1e-6)  # floor after scaled eps, same as reference


def main():
    ck = json.load(open(sys.argv[1], encoding="utf-8"))
    text_path = sys.argv[2]
    if not ck.get("valid"):
        print("checker says invalid:", ck)
        sys.exit(1)

    qs = []
    for sat in ck["saturations"]:
        qs.append(rank(sat["sat"]))
    print(f"checker: valid, total_cost={ck['total_cost']}, {len(qs)} saturation entries")

    want_q = []
    for line in open(text_path, encoding="utf-8"):
        if line.startswith("Q "):
            want_q.append(int(line.split()[1]))
    if len(qs) != len(want_q):
        print(f"length mismatch: checker {len(qs)} vs text {len(want_q)}")
        sys.exit(1)

    bad = 0
    first = None
    for i, (a, b) in enumerate(zip(qs, want_q)):
        if a != b:
            bad += 1
            if first is None:
                first = (i, a, b)
    print(f"compare: {len(qs)} entries, trunc6 mismatches={bad}")
    if first:
        print(f"  first mismatch at #{first[0]} checker={first[1]} text={first[2]}")
    else:
        print("  RESULT: checker == reference vector (trunc6, elementwise) OK")


if __name__ == "__main__":
    main()
