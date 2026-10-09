#!/usr/bin/env python3
"""Ask the running stack the question the README answers.

A telemetry pipeline is eventually consistent: the collector batches, Jaeger indexes, Prometheus scrapes.
Asking once asserts against the speed of the machine, so every check here retries until the pipeline has
caught up - the first version of the CI job that used this failed on a stack that was working.

    python3 scripts/check-telemetry.py                      # against localhost
    JAEGER_URL=http://jaeger:16686 python3 ...             # against a stack somewhere else
"""

import json
import os
import sys
import time
import urllib.request

ATTEMPTS = int(os.environ.get("ATTEMPTS", "40"))
DELAY = float(os.environ.get("DELAY", "5"))
JAEGER_URL = os.environ.get("JAEGER_URL", "http://localhost:16686")
PROMETHEUS_URL = os.environ.get("PROMETHEUS_URL", "http://localhost:9090")


def get(url):
    with urllib.request.urlopen(url, timeout=5) as response:
        return json.load(response)


def a_play_session_is_a_trace(jaeger):
    """The headline claim: a step of a match is a span that says which match and which player."""
    traces = get(f"{jaeger}/api/traces?service=gameserver&limit=10")["data"]
    with_the_match = [
        t for t in traces if any(x["key"] == "game.match.id" for s in t["spans"] for x in s["tags"])
    ]
    if not with_the_match:
        return False, f"{len(traces)} traces, none carrying game.match.id yet"
    return True, f"{len(traces)} traces, {len(with_the_match)} carry the match id"


def the_metrics_are_scraped(prometheus):
    """Both targets: the service, and the collector whose own health is the pipeline's health."""
    targets = get(f"{prometheus}/api/v1/targets")["data"]["activeTargets"]
    if not targets:
        return False, "Prometheus knows no targets yet"
    down = [t["labels"]["job"] for t in targets if t["health"] != "up"]
    if down:
        return False, "down: " + ", ".join(sorted(down))
    return True, "targets up: " + ", ".join(sorted(t["labels"]["job"] for t in targets))


def the_rules_are_loaded(prometheus):
    """An alert rule that failed to compile is an outage nobody hears about."""
    groups = get(f"{prometheus}/api/v1/rules")["data"]["groups"]
    rules = [r for g in groups for r in g["rules"]]
    if not rules:
        return False, "no alert rules are loaded"
    broken = [r["name"] for r in rules if r.get("health") not in (None, "ok")]
    if broken:
        return False, "rules reporting an error: " + ", ".join(broken)
    return True, f"{len(rules)} alert rules loaded and evaluating"


def wait(what, check, url):
    detail = "the check never ran"
    for attempt in range(1, ATTEMPTS + 1):
        try:
            ok, detail = check(url)
        except Exception as error:  # the stack is still coming up, which is not a failure yet
            ok, detail = False, f"{type(error).__name__}: {error}"
        if ok:
            print(f"ok   {what}: {detail}")
            return True
        print(f"..   {what} (attempt {attempt}/{ATTEMPTS}): {detail}")
        time.sleep(DELAY)
    print(f"FAIL {what}: {detail}", file=sys.stderr)
    return False


def main():
    checks = [
        ("a play session is a trace with the match on it", a_play_session_is_a_trace, JAEGER_URL),
        ("the metrics are being scraped", the_metrics_are_scraped, PROMETHEUS_URL),
        ("the alert rules are loaded", the_rules_are_loaded, PROMETHEUS_URL),
    ]
    results = [wait(what, check, url) for what, check, url in checks]
    return 0 if all(results) else 1


if __name__ == "__main__":
    sys.exit(main())
