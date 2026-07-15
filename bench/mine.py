#!/usr/bin/env python3
"""Mine a repo's history for bench-task-shaped commits.

A candidate: subject matches a semantic-edit pattern, touches >=4 Go files,
diff is mostly Go, not huge, no vendor/generated churn.
"""
import json
import re
import subprocess
import sys

PATTERNS = [
    ("rename", re.compile(r"\brenam(e|ed|ing)\b", re.I)),
    ("add-param", re.compile(r"\b(add|pass|plumb|thread|propagate)\b.*\b(ctx|context|param|parameter|argument)\b", re.I)),
    ("signature", re.compile(r"\b(change|update|new)\b.*\bsignature\b", re.I)),
    ("interface", re.compile(r"\b(implement|extract|introduce|satisfy)\b.*\binterface\b", re.I)),
    ("move", re.compile(r"\bmove\b.*\b(package|func|function|type|into|to)\b", re.I)),
    ("wrap-error", re.compile(r"\bwrap\b.*\berr(or)?s?\b", re.I)),
]
SKIP_PATH = re.compile(r"(^|/)(vendor|third_party|node_modules)/|\.pb\.go$|_generated\.go$|\.gen\.go$|zz_generated|mock")


def mine(repo):
    # Phase 1: subject grep needs no blobs (clone is blob-filtered).
    subjects = subprocess.run(
        ["git", "-C", repo, "log", "--no-merges", "--pretty=%H\x02%s"],
        capture_output=True, text=True, check=True).stdout
    matched = [line.split("\x02", 1)[0] for line in subjects.splitlines()
               if line and any(p.search(line.split("\x02", 1)[1]) for _, p in PATTERNS)]
    print(f"{repo}: {len(matched)} subject matches", file=sys.stderr)
    if not matched:
        return []
    # Phase 2: numstat only the matches; lazy blob fetch happens here.
    # Chunked with retry: promisor fetches flake over long runs.
    out = ""
    for i in range(0, len(matched), 25):
        chunk = matched[i:i + 25]
        for attempt in range(3):
            r = subprocess.run(
                ["git", "-C", repo, "log", "--no-walk=unsorted", "--pretty=%x01%H\x02%s", "--numstat", "--stdin"],
                input="\n".join(chunk), capture_output=True, text=True)
            if r.returncode == 0:
                out += r.stdout
                break
            print(f"{repo}: chunk {i} attempt {attempt}: {r.stderr.strip()[:200]}", file=sys.stderr)
        else:
            print(f"{repo}: chunk {i} failed, skipping {len(chunk)} commits", file=sys.stderr)
    candidates = []
    for block in out.split("\x01"):
        if not block.strip():
            continue
        header, _, rest = block.partition("\n")
        sha, _, subject = header.partition("\x02")
        kinds = [k for k, p in PATTERNS if p.search(subject)]
        if not kinds:
            continue
        go, other, skipped, churn = 0, 0, 0, 0
        for line in rest.splitlines():
            parts = line.split("\t")
            if len(parts) != 3:
                continue
            adds, dels, path = parts
            if SKIP_PATH.search(path):
                skipped += 1
                continue
            if path.endswith(".go"):
                go += 1
                churn += int(adds if adds != "-" else 0) + int(dels if dels != "-" else 0)
            else:
                other += 1
        total = go + other
        if go >= 4 and total and go / total >= 0.7 and churn <= 1500 and skipped <= 2:
            candidates.append({
                "sha": sha, "subject": subject, "kinds": kinds,
                "go_files": go, "other_files": other, "churn": churn,
            })
    return candidates


if __name__ == "__main__":
    repo = sys.argv[1]
    cands = mine(repo)
    cands.sort(key=lambda c: c["go_files"])
    json.dump({"repo": repo, "count": len(cands), "candidates": cands},
              sys.stdout, indent=1)
    print()
