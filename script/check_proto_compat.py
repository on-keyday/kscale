#!/usr/bin/env python3
"""Fail if a .proto change breaks wire compatibility with the committed version.

Protobuf identifies fields on the wire by NUMBER, and a node running an older
build keeps sending the old numbers. The generators derive numbers from
declaration order, so an inserted field silently renumbers everything after it:
85d4fe8 did exactly that to stat.Stats, and a CP on the new numbering misread
every not-yet-upgraded node (notes/bugs/bug_2026_09_25_stats_proto_renumbered.md).

For every tracked .proto (or the paths given), this compares each message /
oneof / extend field and each enum value against the same file at a base git
revision (default HEAD) and reports:

  ERROR  an existing field or enum value got a different number
  ERROR  a number now carries a field/value of a different type (reuse)
  WARN   a number now carries a differently NAMED field of the same type (a
         rename: wire-compatible, but JSON/CLI names change)
  WARN   a field/value was removed (reserve its number so it is never reused)

New fields and new files are fine. Exit 1 on any ERROR.

  script/check_proto_compat.py [--base REV] [FILE.proto ...]
  PROTO_COMPAT_BASE=REV  same as --base
  PROTO_COMPAT_SKIP=1    skip the check (an intentional, coordinated break)
"""
import os
import re
import subprocess
import sys

FIELD = re.compile(r"^\s*(?:optional\s+|repeated\s+)?(map<[^>]+>|[\w.]+)\s+(\w+)\s*=\s*(\d+)\s*[;\[]")
ENUM_VALUE = re.compile(r"^\s*(\w+)\s*=\s*(-?\d+)\s*[;\[]")
BLOCK = re.compile(r"^\s*(message|enum|oneof|extend|service)\s+([\w.]+)\s*\{")


def parse(text):
    """-> {container: {number: (name, type)}} for message/oneof/extend fields and
    enum values. A oneof's fields belong to its enclosing message (one number
    space), so they are recorded under the message."""
    out = {}
    stack = []  # (kind, name)
    for raw in text.splitlines():
        line = raw.split("//", 1)[0]
        m = BLOCK.match(line)
        if m:
            stack.append((m.group(1), m.group(2)))
            # a one-line block ("message X {}") closes on the same line
            if line.count("}") >= line.count("{"):
                stack.pop()
            continue
        if stack:
            kind = stack[-1][0]
            owner = [n for k, n in stack if k != "oneof"]
            container = ".".join(owner) if owner else ""
            if kind in ("message", "oneof", "extend"):
                f = FIELD.match(line)
                if f:
                    typ, name, num = f.group(1), f.group(2), int(f.group(3))
                    out.setdefault(container, {})[num] = (name, typ)
            elif kind == "enum":
                e = ENUM_VALUE.match(line)
                if e:
                    out.setdefault("enum " + container, {})[int(e.group(2))] = (e.group(1), "enum")
        for _ in range(line.count("}")):
            if stack:
                stack.pop()
    return out


def base_text(rev, path):
    try:
        return subprocess.run(["git", "show", f"{rev}:{path}"], check=True,
                              capture_output=True, text=True).stdout
    except subprocess.CalledProcessError:
        return None  # new file (or not in rev): nothing to break


def compare(path, old, new):
    errors, warns = [], []
    for container, oldf in old.items():
        newf = new.get(container)
        where = f"{path}: {container}"
        if newf is None:
            warns.append(f"{where}: removed ({len(oldf)} numbers now free — reserve them)")
            continue
        new_by_name = {name: num for num, (name, _) in newf.items()}
        for num, (name, typ) in sorted(oldf.items()):
            if num in newf:
                nname, ntyp = newf[num]
                if ntyp != typ:
                    errors.append(f"{where}: number {num} was {typ} {name}, now {ntyp} {nname} (reused with another type)")
                elif nname != name:
                    if name in new_by_name:
                        errors.append(f"{where}: {name} moved {num} -> {new_by_name[name]} (and {num} is now {nname})")
                    else:
                        warns.append(f"{where}: number {num} renamed {name} -> {nname}")
            elif name in new_by_name:
                errors.append(f"{where}: {name} renumbered {num} -> {new_by_name[name]}")
            else:
                warns.append(f"{where}: {name} = {num} removed (reserve {num})")
    return errors, warns


def main(argv):
    if os.environ.get("PROTO_COMPAT_SKIP") == "1":
        print("proto compat: skipped (PROTO_COMPAT_SKIP=1)")
        return 0
    base = os.environ.get("PROTO_COMPAT_BASE", "HEAD")
    args = list(argv)
    if args[:1] == ["--base"]:
        base, args = args[1], args[2:]
    if subprocess.run(["git", "rev-parse", "--verify", "-q", base], capture_output=True).returncode != 0:
        print(f"proto compat: base {base!r} not found (no git history?) — skipped")
        return 0
    paths = args or subprocess.run(
        ["git", "ls-files", "*.proto"], check=True, capture_output=True, text=True).stdout.split()
    errors, warns = [], []
    for path in paths:
        old = base_text(base, path)
        if old is None or not os.path.exists(path):
            continue
        with open(path) as f:
            e, w = compare(path, parse(old), parse(f.read()))
        errors += e
        warns += w
    for w in warns:
        print("WARN  " + w)
    for e in errors:
        print("ERROR " + e)
    if errors:
        print(f"proto compat: {len(errors)} wire-breaking change(s) vs {base}. Append new fields with new "
              "numbers instead (for generated stat categories: appended_categories). "
              "PROTO_COMPAT_SKIP=1 only for a deliberate, fleet-wide coordinated break.")
        return 1
    print(f"proto compat: OK vs {base} ({len(paths)} files, {len(warns)} warning(s))")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
