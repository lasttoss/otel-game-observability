# The metrics that turn an incident into a graph

Three incidents' worth of hindsight, written as the panels and queries that would have made each one
obvious in minutes instead of hours. Every item here is a number that was missing when it mattered.

## 1. A connection count that must be flat, not trending

A cache client created on the request path makes the connection count grow with *matches*, not with
*pods*. Plot `connected_clients` **per pod** and read the shape:

- flat, proportional to the number of pods — healthy;
- rising with rooms or matches — a leak by construction, whatever the code review said.

Alert on **headroom against the server's client limit**, not on the limit being reached, because by
the time the limit is reached the cache refuses new connections and the application fails in a way
that looks like a network problem. The supporting panels that let you rule out the neighbours in one
screen: database connections in use, database deadlocks and slow queries, node load, and the
conntrack table against its maximum.

## 2. Throttling is invisible unless you read the cgroup

```bash
kubectl top pod -n <ns>
kubectl exec <pod> -c <container> -- cat /sys/fs/cgroup/cpu.stat
```

`nr_throttled > 0` means the number you are looking at is the **limit**, not the server. In one
measurement an application container showed 1,314 throttled periods (≈70 s of cumulative
throttling) while the node sat at 72% utilisation — the graph that mattered was the cgroup's, not
the node's. Export `cpu.stat` per container; a node-level CPU graph cannot show it.

## 3. Per-component cost, or the wrong component gets blamed

CPU per 1,000 players, measured for the application, the realtime UDP tier and the ingress, at two
client traffic profiles:

| | quiet client (0.75 msg/bot/s) | busy client (3.7 msg/bot/s) |
|---|---|---|
| application | 0.28 – 0.33 core | **0.92** |
| realtime UDP tier | 0.14 | **0.66** |
| ingress | 0.23 | **0.74** |

Two panels follow from this: per-container CPU against its **limit** (so a component pinned at its
cap is visible before it degrades), and the same normalised per 1,000 players (so a component that
gets expensive with load is visible before it becomes the incident). A single node-level graph hides
which of the three is eating the budget.

## 4. Business errors are not transport errors

A load run reported 139 errors and none of them was a stability problem: every one was
`{"code":5,"message":"match full"}` — a full lobby, which is the game working. Count them in
separate series:

- **transport**: closed sockets, timeouts, refused connections, unparsed frames;
- **business**: "match full", "already in progress", validation rejections.

An alerting rule that adds them together will page you for a successful product. The same split
belongs in the load-test report, which is why the harness in `go-loadtest-swarm` asserts on them
separately.

## 5. Latency where the player feels it

Track p50 and p99 for **auth and join**, not for HTTP in general: a player experience is a login
that takes 900 ms plus a match join that takes 1,200 ms, and both disappear inside an aggregate that
mostly measures health checks. Keep message and byte rates next to them — a latency plateau that
comes with a flat message rate is backpressure, and one that comes with a rising rate is load.
