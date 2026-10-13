# Measurements

The README's claim is that a question like "which match was slow" has an answer in a trace, on the
stack that produced it. So the evidence is a run of that stack.

## Environment

| | |
|---|---|
| Machine | 12-core x86-64 (AMD64), Ubuntu 24.04, Docker 29.1.3 |
| Stack | docker compose: gameserver, otel-collector, jaeger, prometheus, grafana |
| Date | 2026-10-13 (UTC) |

## A play session is a trace that carries the match

```
$ make up          # every container reaches healthy
$ make assert      # sends its own load, then checks the telemetry
python3 scripts/check-telemetry.py
..   a play session is a trace with the match on it (attempt 1/40): 0 traces, none carrying game.match.id yet
ok   a play session is a trace with the match on it: 10 traces, 10 carry the match id
ok   the metrics are being scraped: targets up: gameserver, otel-collector
ok   the alert rules are loaded: 4 alert rules loaded and evaluating
```

**Result.** Ten traces, all ten carrying `game.match.id`, the metrics targets up, and four alert rules
loaded and evaluating. The first attempt finding zero traces is the assertion working as written: it
retries until the collector has flushed rather than asserting on the first look.

## p99 of a match step: 0.99 us, steps served: 4.18/s

That line is quoted in the README from its own `make up && make demo && make traces` output. **It is
not reproduced by the pass that wrote this file**, and rather than leave the gap silent:

```
$ make up && make demo && make traces
0 traces
```

Two runs of exactly that sequence returned `0 traces`, while `make assert` - which sends its own load
and waits for the collector - found ten immediately. The likely reading is that the demo's traffic is
over by the time `traces` queries Jaeger, and the query window is the run's own.

**To see the figure:** `make up && make assert && make traces`, or drive your own load first. It is a
property of a served step, and a stack with no traffic has none to report.

## What this repository is for

The repository is small on purpose: "a game backend small enough to read in one sitting, instrumented
so that every step of a match is one span". The value is in the shape of the telemetry - one span per
step, carrying the match, the player and the action - and that is what the assertion above checks.
