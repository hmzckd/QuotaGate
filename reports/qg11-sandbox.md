# QG-11 load report

- Run: 2026-10-12 15:17:43 to 15:28:03 UTC
- Code: commit `1b82b29+dirty`, go1.27.0 linux/amd64, 4 CPUs (Intel(R) Xeon(R) Processor @ 2.10GHz)
- Environment: cloud sandbox VM, Linux; QuotaGate serve-combined, both example APIs, PostgreSQL 16 and Redis as local processes (no Docker)
- Load per target: 50 req/s for 5m0s, at most 20 in flight, seed 11, 10s unmeasured warm-up
- Target, fixed before the run: p95 latency added by the middleware < 30ms

## Latency

Client side, from the scheduled send time to the end of the response. Both runs send the same seeded plan to the same example API handler; the only difference is the QuotaGate middleware.

| | Requests | Achieved rate | p50 | p95 | p99 | Max | Errors |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Baseline, no middleware | 15000 | 50.0/s | 1.4 ms | 2.3 ms | 15.5 ms | 164.6 ms | 0 (0.00%) |
| QuotaGate middleware | 15000 | 50.0/s | 4.8 ms | 7.6 ms | 21.4 ms | 143.3 ms | 0 (0.00%) |
| Added by the middleware | | | 3.4 ms | 5.3 ms | 5.9 ms | | |

Target: **met**. p95 added 5.3 ms, target < 30ms.

## Decisions in the QuotaGate run

| Customer | Kind | Daily limit | Per minute | Requests | 201 | 429 daily_quota | 429 rate_limited | Other |
| ---: | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | open | 1000000000 | 1000000 | 1276 | 1276 | 0 | 0 | 0 |
| 2 | open | 1000000000 | 1000000 | 1379 | 1379 | 0 | 0 | 0 |
| 3 | open | 1000000000 | 1000000 | 1281 | 1281 | 0 | 0 | 0 |
| 4 | open | 1000000000 | 1000000 | 1305 | 1305 | 0 | 0 | 0 |
| 5 | open | 1000000000 | 1000000 | 1319 | 1319 | 0 | 0 | 0 |
| 6 | open | 1000000000 | 1000000 | 1296 | 1296 | 0 | 0 | 0 |
| 7 | open | 1000000000 | 1000000 | 1355 | 1355 | 0 | 0 | 0 |
| 8 | open | 1000000000 | 1000000 | 1316 | 1316 | 0 | 0 | 0 |
| 9 | daily | 500 | 1000000 | 2239 | 500 | 1739 | 0 | 0 |
| 10 | rate | 1000000000 | 120 | 2234 | 632 | 0 | 1602 | 0 |

Warm-up requests went to open customers only; they are left out of this table and the latency, but included in the checks below.

## Correctness checks

Client responses compared with PostgreSQL decisions and daily usage, and with the example API's handler count.

- PASS every request got 201 or a known 429: 0 other responses
- PASS handler ran once per 201: handler calls 12159, 201 responses 12159
- PASS customer 1 (open): one decision record per request: records 1339, requests 1339
- PASS customer 1 (open): records match responses: allowed/daily/rate records 1339/0/0, responses 1339/0/0
- PASS customer 1 (open): daily usage equals allowed: daily usage 1339, allowed 1339
- PASS customer 1 (open): every request allowed: allowed 1339 of 1339
- PASS customer 2 (open): one decision record per request: records 1442, requests 1442
- PASS customer 2 (open): records match responses: allowed/daily/rate records 1442/0/0, responses 1442/0/0
- PASS customer 2 (open): daily usage equals allowed: daily usage 1442, allowed 1442
- PASS customer 2 (open): every request allowed: allowed 1442 of 1442
- PASS customer 3 (open): one decision record per request: records 1344, requests 1344
- PASS customer 3 (open): records match responses: allowed/daily/rate records 1344/0/0, responses 1344/0/0
- PASS customer 3 (open): daily usage equals allowed: daily usage 1344, allowed 1344
- PASS customer 3 (open): every request allowed: allowed 1344 of 1344
- PASS customer 4 (open): one decision record per request: records 1368, requests 1368
- PASS customer 4 (open): records match responses: allowed/daily/rate records 1368/0/0, responses 1368/0/0
- PASS customer 4 (open): daily usage equals allowed: daily usage 1368, allowed 1368
- PASS customer 4 (open): every request allowed: allowed 1368 of 1368
- PASS customer 5 (open): one decision record per request: records 1381, requests 1381
- PASS customer 5 (open): records match responses: allowed/daily/rate records 1381/0/0, responses 1381/0/0
- PASS customer 5 (open): daily usage equals allowed: daily usage 1381, allowed 1381
- PASS customer 5 (open): every request allowed: allowed 1381 of 1381
- PASS customer 6 (open): one decision record per request: records 1358, requests 1358
- PASS customer 6 (open): records match responses: allowed/daily/rate records 1358/0/0, responses 1358/0/0
- PASS customer 6 (open): daily usage equals allowed: daily usage 1358, allowed 1358
- PASS customer 6 (open): every request allowed: allowed 1358 of 1358
- PASS customer 7 (open): one decision record per request: records 1417, requests 1417
- PASS customer 7 (open): records match responses: allowed/daily/rate records 1417/0/0, responses 1417/0/0
- PASS customer 7 (open): daily usage equals allowed: daily usage 1417, allowed 1417
- PASS customer 7 (open): every request allowed: allowed 1417 of 1417
- PASS customer 8 (open): one decision record per request: records 1378, requests 1378
- PASS customer 8 (open): records match responses: allowed/daily/rate records 1378/0/0, responses 1378/0/0
- PASS customer 8 (open): daily usage equals allowed: daily usage 1378, allowed 1378
- PASS customer 8 (open): every request allowed: allowed 1378 of 1378
- PASS customer 9 (daily): one decision record per request: records 2239, requests 2239
- PASS customer 9 (daily): records match responses: allowed/daily/rate records 500/1739/0, responses 500/1739/0
- PASS customer 9 (daily): daily usage equals allowed: daily usage 500, allowed 500
- PASS customer 9 (daily): exactly the daily limit allowed: allowed 500, want 500; rate_limited 0
- PASS customer 10 (rate): one decision record per request: records 2234, requests 2234
- PASS customer 10 (rate): records match responses: allowed/daily/rate records 632/0/1602, responses 632/0/1602
- PASS customer 10 (rate): daily usage equals allowed: daily usage 632, allowed 632
- PASS customer 10 (rate): per-minute limit kept: allowed 632, possible 625..720; daily_quota 0

## What this does not show

A local run with the load generator, both example APIs, QuotaGate, PostgreSQL and Redis on one machine. It compares the middleware with its own baseline under the same conditions. It is not a production capacity, HA or general p95 SLA figure.
