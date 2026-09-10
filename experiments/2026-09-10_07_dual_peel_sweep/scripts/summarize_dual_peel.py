#!/usr/bin/env python3
"""Parse -dual-probe/-dual-peel logs into the layer table used by report.md.

The logs are the raw stdout of:
  solution/bin/solve.exe -prefix ../setA/setA-NN -rounds 1 \
      -dual-probe 1 -dual-peel 3 -schedule hot -conf-gate=false \
      -wall-sec 90 -mip-sec 10
One file per instance, named data/inv-NN.log.

Two invariants are checked rather than assumed, because both are what make the
prices trustworthy:
  A  layer 0 has no live price  <=>  z sits on its own lower bound
  B  sumLcap == 1               <=>  z is interior (not at its bound)
sumLcap = sum(|Pi| * cap) over the rows that still carry z; stationarity in z
forces it to 1, so it is a numerical check on both the binding and the reading.
"""
import re, glob, os, csv, sys

D = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
GAP = 'experiments/2026-09-09_m3_twin_mip_setA_bench/data/gap_table.tsv'

PEEL = (r'^    L(\d) zlb=([\d.]+) z=([\d.]+) \(sound floor [\d.]+, \+(-?[\d.]+)\) '
        r'priced=\d+ live=(\d+).*?sumLcap=([\d.]+) disagree=(\d+)/(\d+)')
FREE = (r'\[L0free\] z=([\d.]+) \(sound floor ([\d.]+), \+(-?[\d.]+)\) '
        r'priced=\d+ live=(\d+).*?sumLcap=([\d.]+) disagree=(\d+)/(\d+)')


def load():
    ref = {}
    try:
        with open(GAP, encoding='utf-8') as fh:
            for d in csv.DictReader(fh, delimiter='\t'):
                ref[d['instance']] = d
    except OSError:
        pass
    data = {}
    for f in sorted(glob.glob(os.path.join(D, 'data', 'inv-*.log'))):
        n = 'setA-' + os.path.basename(f)[4:6]
        txt = open(f, encoding='utf-8', errors='replace').read()
        m = re.search(FREE, txt)
        if not m:
            data[n] = None
            continue
        d = {'free': dict(z=float(m.group(1)), floor=float(m.group(2)),
                          live=int(m.group(4)), sum=float(m.group(5)),
                          dis=(int(m.group(6)), int(m.group(7)))), 'peel': {}}
        for g in re.finditer(PEEL, txt, re.M):
            d['peel'][int(g.group(1))] = dict(
                zlb=float(g.group(2)), z=float(g.group(3)), live=int(g.group(5)),
                sum=float(g.group(6)), dis=(int(g.group(7)), int(g.group(8))))
        data[n] = d
    return data, ref


def main():
    data, ref = load()
    print("rule A  [L0 live==0 <=> z on zlb]:", end=' ')
    ok = bad = 0
    for n, d in sorted(data.items()):
        if not d or 0 not in d['peel']:
            continue
        e = d['peel'][0]
        if (abs(e['zlb'] - e['z']) < 1e-9) == (e['live'] == 0):
            ok += 1
        else:
            bad += 1
            print(f"VIOLATION {n} {e}", file=sys.stderr)
    print(f"{ok}/{ok+bad}")

    print("rule B  [sumLcap==1 <=> z interior]:", end=' ')
    ok = tot = 0
    for n, d in sorted(data.items()):
        if not d:
            continue
        checks = []
        if d['free']['z'] > 0:
            checks.append((True, d['free']['sum'] == 1.0, 'L0free'))
        for L, e in d['peel'].items():
            checks.append((abs(e['zlb'] - e['z']) >= 1e-9, e['sum'] == 1.0, f'L{L}'))
        for want, got, lbl in checks:
            tot += 1
            if want == got:
                ok += 1
            else:
                print(f"VIOLATION {n} {lbl}", file=sys.stderr)
    print(f"{ok}/{tot}")

    print("\ndisagreement by max_parity (from the 2026-09-09 m3 bench):")
    for g in ('NOT', 'tie'):
        gs = []
        for n, d in sorted(data.items()):
            if not d:
                continue
            dis = [('L0free',) + d['free']['dis']]
            for L in sorted(d['peel']):
                dis.append((f'L{L}',) + d['peel'][L]['dis'])
            if ref.get(n, {}).get('max_parity') == g:
                gs.append((n, [x for x in dis if x[1] != 0]))
        nz = [x for x in gs if x[1]]
        print(f"  {g}: {len(nz)}/{len(gs)} have >=1 disagreeing layer")
        for n, dis in nz:
            print(f"      {n}: " + " ".join(f"{a}={b}/{c}" for a, b, c in dis))


if __name__ == '__main__':
    main()
