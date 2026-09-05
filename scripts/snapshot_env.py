"""Write a reproducible code/environment snapshot into an experiment folder.

Writes ``<dir>/meta.json`` with the git branch/commit, dirty-file list, and
Python/library versions so that every experiment report is traceable to a
code version.  Run from anywhere:

    python scripts/snapshot_env.py experiments/2026-09-06_exp02_checker_xval
"""
import datetime
import json
import subprocess
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parents[1]


def git(*args: str) -> str:
    try:
        r = subprocess.run(["git", *args], capture_output=True, text=True,
                           cwd=REPO, timeout=30)
        return r.stdout.strip() if r.returncode == 0 else "n/a"
    except Exception:
        return "n/a"


def main() -> None:
    outdir = Path(sys.argv[1]) if len(sys.argv) > 1 else Path(".")
    outdir.mkdir(parents=True, exist_ok=True)
    dirty = git("status", "--porcelain")
    meta = {
        "timestamp": datetime.datetime.now().isoformat(timespec="seconds"),
        "git_branch": git("branch", "--show-current"),
        "git_commit": git("rev-parse", "HEAD"),
        "git_short": git("rev-parse", "--short", "HEAD"),
        "git_dirty_file_count": len([l for l in dirty.splitlines() if l]),
        "git_dirty_files": [l[:2] + " " + l[3:] for l in dirty.splitlines()],
        "python": sys.version.split()[0],
    }
    for pkg in ("numpy",):
        try:
            mod = __import__(pkg)
            meta[pkg] = getattr(mod, "__version__", "n/a")
        except Exception:
            meta[pkg] = "not-installed"
    target = outdir / "meta.json"
    target.write_text(json.dumps(meta, indent=2, ensure_ascii=False) + "\n")
    print(f"wrote {target}")


if __name__ == "__main__":
    main()
