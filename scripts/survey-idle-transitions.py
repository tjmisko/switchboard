#!/usr/bin/env python3
"""Survey idle intervals using history metadata only; this is not an idle oracle."""

import argparse
import collections
import datetime
import json
import pathlib


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("history_dir", type=pathlib.Path)
    parser.add_argument("--since", required=True, help="First day, YYYY-MM-DD")
    parser.add_argument("--until", required=True, help="Last day, YYYY-MM-DD (inclusive)")
    args = parser.parse_args()
    first = datetime.date.fromisoformat(args.since)
    last = datetime.date.fromisoformat(args.until)
    if first > last:
        parser.error("--since must not be later than --until")
    prior = {}
    rules = collections.Counter()
    gaps = collections.defaultdict(list)
    count = 0
    for path in sorted(args.history_dir.glob("????-??-??.jsonl")):
        try:
            day = datetime.date.fromisoformat(path.stem)
        except ValueError:
            continue
        if not first <= day <= last:
            continue
        with path.open() as stream:
            for line in stream:
                try:
                    event = json.loads(line)
                    if event.get("type") != "transition":
                        continue
                    at = datetime.datetime.fromisoformat(event["ts"].replace("Z", "+00:00"))
                    if at.tzinfo is None:
                        continue
                except (ValueError, KeyError, AttributeError):
                    continue
                count += 1
                rule = event.get("rule") or "unattributed"
                rules[(event.get("agent", ""), rule)] += 1
                # IDs are joins in memory only, never output.
                key = (event.get("agent"), event.get("session_id") or event.get("pid"))
                previous = prior.get(key)
                if previous and previous[0] == "idle" and event.get("from") == "idle" and event.get("to") in ("working", "delegating"):
                    gap = (at - previous[1]).total_seconds()
                    if 0 <= gap <= 120:
                        gaps[previous[2]].append(gap)
                prior[key] = (event.get("to"), at, rule)
    print(json.dumps({
        "since": args.since, "until": args.until, "transition_count": count,
        "rules": [{"agent": agent, "rule": rule, "count": n} for (agent, rule), n in rules.most_common()],
        "idle_returns_within_120s": [{
            "rule": rule, "count": len(values), "min_s": min(values),
            "median_s": sorted(values)[len(values) // 2], "max_s": max(values),
            "under_5s": sum(value < 5 for value in values),
        } for rule, values in sorted(gaps.items())],
    }, indent=2))


if __name__ == "__main__":
    main()
