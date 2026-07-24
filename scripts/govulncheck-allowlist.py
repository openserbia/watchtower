#!/usr/bin/env python3
"""Run govulncheck and gate on it, minus an allowlist of known-but-unfixable advisories.

govulncheck has no native ignore file, so this wrapper runs it in JSON mode,
collects the advisories whose vulnerable symbols are actually *reachable* from our
code (call-graph aware), and exits non-zero only for ones NOT in the allowlist.

Exit codes:
  0  every reachable vulnerability is allowlisted (or there are none)
  1  a new / un-allowlisted reachable vulnerability, OR govulncheck failed to run

It also surfaces two maintenance signals as warnings (without failing):
  - a fix has become available for an allowlisted advisory  -> bump the dep, drop the entry
  - an allowlisted advisory is no longer reachable           -> stale entry, remove it

Usage: python3 scripts/govulncheck-allowlist.py [packages...]   (default ./...)
"""

import json
import subprocess
import sys

# Allowlisted advisories (reviewed 2026-07-24). Currently empty: the two former
# entries (GO-2026-4887, GO-2026-4883) were daemon-side Moby bugs in
# github.com/docker/docker@v28.5.2+incompatible that govulncheck could only flag
# at module level. The Moby v29 client migration replaced that module path, so
# those advisories are no longer imported or reachable and have been dropped.
# Add an entry here only for a reachable advisory that has no available fix,
# with a note explaining why it is not exercised; drop it once the wrapper
# reports a fix has become available or that it is no longer reachable.
ALLOW: dict[str, str] = {}


def main() -> int:
    pkgs = sys.argv[1:] or ["./..."]
    proc = subprocess.run(
        ["govulncheck", "-format", "json", *pkgs],
        capture_output=True,
        text=True,
        check=False,
    )
    # govulncheck exits 0 (no vulns) or 3 (vulns found) on a successful run; any
    # other code means it could not analyze the code (build/config error) — that
    # must fail loudly rather than be mistaken for "no vulnerabilities".
    if proc.returncode not in (0, 3):
        sys.stderr.write(proc.stderr)
        sys.stderr.write(f"\ngovulncheck could not run (exit {proc.returncode})\n")
        return 1

    # JSON mode emits a stream of concatenated objects; a vulnerability is
    # reachable when one of its findings carries a symbol-level trace frame.
    reachable: dict[str, str] = {}
    decoder = json.JSONDecoder()
    data, idx, end = proc.stdout, 0, len(proc.stdout)
    while idx < end:
        while idx < end and data[idx].isspace():
            idx += 1
        if idx >= end:
            break
        obj, idx = decoder.raw_decode(data, idx)
        finding = obj.get("finding")
        if not finding:
            continue
        trace = finding.get("trace") or []
        if any(frame.get("function") for frame in trace):
            reachable[finding["osv"]] = finding.get("fixed_version") or ""

    unexpected = sorted(set(reachable) - set(ALLOW))
    allowed = sorted(set(reachable) & set(ALLOW))
    stale = sorted(set(ALLOW) - set(reachable))

    for osv in allowed:
        fixed = reachable[osv]
        suffix = f"  >> NOW FIXED IN {fixed}: bump the dep and drop this entry" if fixed else ""
        print(f"allowlisted: {osv} — {ALLOW[osv]}{suffix}")
    for osv in stale:
        print(f"stale allowlist: {osv} no longer reachable — remove it from ALLOW")

    if unexpected:
        print("\nFAIL: reachable vulnerabilities not in the allowlist:")
        for osv in unexpected:
            print(f"  - {osv}  (fixed in: {reachable[osv] or 'N/A'})  https://pkg.go.dev/vuln/{osv}")
        return 1

    print(f"\nOK: {len(allowed)} allowlisted, no new reachable vulnerabilities")
    return 0


if __name__ == "__main__":
    sys.exit(main())
