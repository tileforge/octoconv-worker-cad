#!/usr/bin/env python3
"""vtk_keeplist.py -- generate a VTK keep-list from process maps and prune a venv by it.

Usage:
    vtk_keeplist.py collect --vtk-version V --out FILE MAPS...
    vtk_keeplist.py prune --keeplist FILE --site DIR

Entries are architecture-neutral: the ".cpython-311-<arch>-linux-gnu" tag of an
extension module is dropped when collecting and when matching, so one list serves
arm64 and amd64.

Exit codes: 1 stale keep-list or nothing collected, 64 usage.
"""

import argparse
import os
import re
import sys

SCOPE_DIRS = ("vtkmodules", "vtk.libs")
SITE_MARK = "/site-packages/"
EXT_TAG = re.compile(r"\.cpython-[0-9]+[a-z]*-[A-Za-z0-9_]+-linux-gnu(?=\.so)")


def normalize(rel):
    return EXT_TAG.sub("", rel)


def relative_entry(path):
    path = path.strip().replace(" (deleted)", "")
    cut = path.find(SITE_MARK)
    if cut < 0:
        return None
    rel = path[cut + len(SITE_MARK):]
    top = rel.split("/", 1)[0]
    if top not in SCOPE_DIRS or ".so" not in rel.rsplit("/", 1)[-1]:
        return None
    return normalize(rel)


def collect(args):
    entries = set()
    for maps in args.maps:
        with open(maps) as fh:
            for line in fh:
                rel = relative_entry(line)
                if rel:
                    entries.add(rel)
    if not entries:
        print("vtk_keeplist: no vtkmodules path in the maps", file=sys.stderr)
        return 1
    scopes = [d for d in SCOPE_DIRS if any(e.startswith(d + "/") for e in entries)]
    with open(args.out, "w") as fh:
        fh.write("vtk_version=%s\n" % args.vtk_version)
        fh.write("scope=%s\n" % ",".join(scopes))
        for entry in sorted(entries):
            fh.write(entry + "\n")
    print("COLLECTED,entries=%d,scope=%s" % (len(entries), ",".join(scopes)))
    return 0


def read_keeplist(path):
    with open(path) as fh:
        lines = [ln.rstrip("\n") for ln in fh]
    if len(lines) < 3 or not lines[0].startswith("vtk_version=") or not lines[1].startswith("scope="):
        print("keep-list stale: malformed header in %s" % path, file=sys.stderr)
        sys.exit(1)
    return lines[0].split("=", 1)[1], lines[1].split("=", 1)[1].split(","), [ln for ln in lines[2:] if ln]


def prune(args):
    import importlib.metadata

    version, scopes, entries = read_keeplist(args.keeplist)
    installed = importlib.metadata.version("vtk")
    if version != installed:
        print("keep-list stale: generated for vtk %s, installed %s" % (version, installed), file=sys.stderr)
        return 1
    site = os.path.realpath(args.site)
    keep = {normalize(entry) for entry in entries}
    present = set()
    candidates = []
    for scope in scopes:
        for dirpath, _dirs, files in os.walk(os.path.join(site, scope)):
            for name in files:
                if ".so" not in name:
                    continue
                full = os.path.join(dirpath, name)
                candidates.append(full)
                if os.path.exists(full):
                    present.add(normalize(os.path.relpath(full, site)))
    missing = sorted(keep - present)
    if missing:
        print("keep-list stale: missing %s" % missing[0], file=sys.stderr)
        return 1
    kept = removed = freed = 0
    for full in candidates:
        rel = normalize(os.path.relpath(full, site))
        real = normalize(os.path.relpath(os.path.realpath(full), site))
        if rel in keep or real in keep:
            kept += 1
            continue
        size = os.lstat(full).st_size
        os.unlink(full)
        removed += 1
        freed += size
    print("PRUNED,kept=%d,removed=%d,bytes_removed=%d" % (kept, removed, freed))
    return 0


def main(argv):
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    sub = ap.add_subparsers(dest="cmd", required=True)
    c = sub.add_parser("collect")
    c.add_argument("--vtk-version", required=True)
    c.add_argument("--out", required=True)
    c.add_argument("maps", nargs="+")
    p = sub.add_parser("prune")
    p.add_argument("--keeplist", required=True)
    p.add_argument("--site", required=True)
    args = ap.parse_args(argv[1:])
    return collect(args) if args.cmd == "collect" else prune(args)


if __name__ == "__main__":
    sys.exit(main(sys.argv))
