# QG-13 fault demo: redis outage

- Run: 2026-10-12 21:22:57 to 21:24:27 UTC
- Code: commit `290044b+dirty`, go1.27.0 linux/amd64, Docker 29.8.2
- Environment: Linux cloud VM, 4 CPUs (Intel Xeon 2.10GHz), Docker 29.8.2 in the same VM, run 1 of 2
- Compose project `quotagate-qg13` from compose.yaml, compose.qg08.yaml, compose.qg13.yaml and the local override qg13-sandbox-build.yaml: PostgreSQL, Redis, two QuotaGate copies (`serve-combined`), one example API in front of each copy
- Load: 20 req/s for 1m30s, open loop, half to each example API, at most 32 in flight per copy; the two customers take turns
- Outage: `docker compose stop redis` at +30.0s, `docker compose up` at +55.0s

| Customer | Daily limit | Per minute |
| --- | ---: | ---: |
| basic | 100 | 60 |
| pro | 100000 | 1200 |

## Timeline

Seconds after the load started.

| Time | Event |
| ---: | --- |
| +0.0s | load starts |
| +30.0s | `docker compose stop redis` |
| +30.4s | redis stopped |
| +30.9s | copy 1 `/health/ready` = 503 |
| +30.9s | copy 2 `/health/ready` = 503 |
| +33.9s | in-flight requests drained; handler count 393 |
| +55.0s | handler count 393; `docker compose up --wait redis` |
| +55.7s | copy 2 `/health/ready` = 200 |
| +55.7s | copy 1 `/health/ready` = 200 |
| +59.0s | `up --wait` returned: redis healthy |
| +89.9s | load ends |

## Responses every 5 seconds

By the time the request was sent.

| Sent | Requests | 201 | 429 | 503 | Other |
| ---: | ---: | ---: | ---: | ---: | ---: |
| +0s | 100 | 100 | 0 | 0 | 0 |
| +5s | 100 | 90 | 10 | 0 | 0 |
| +10s | 100 | 50 | 50 | 0 | 0 |
| +15s | 100 | 50 | 50 | 0 | 0 |
| +20s | 100 | 50 | 50 | 0 | 0 |
| +25s | 100 | 50 | 50 | 0 | 0 |
| +30s | 100 | 3 | 3 | 94 | 0 |
| +35s | 100 | 0 | 0 | 100 | 0 |
| +40s | 100 | 0 | 0 | 100 | 0 |
| +45s | 100 | 0 | 0 | 100 | 0 |
| +50s | 100 | 0 | 0 | 100 | 0 |
| +55s | 100 | 54 | 34 | 12 | 0 |
| +60s | 100 | 50 | 50 | 0 | 0 |
| +65s | 100 | 50 | 50 | 0 | 0 |
| +70s | 100 | 50 | 50 | 0 | 0 |
| +75s | 100 | 50 | 50 | 0 | 0 |
| +80s | 100 | 50 | 50 | 0 | 0 |
| +85s | 100 | 50 | 50 | 0 | 0 |

## Decisions per customer

Client responses, then what PostgreSQL recorded. An uncertain 503 was in flight while redis went down or came back.

| Customer | Requests | 201 | 429 daily_quota | 429 rate_limited | 503 (uncertain) | Records | Allowed | daily_quota | rate_limited | daily_usage |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| basic | 900 | 100 | 110 | 437 | 253 (9) | 647 | 100 | 110 | 437 | 100 |
| pro | 900 | 647 | 0 | 0 | 253 (9) | 647 | 647 | 0 | 0 | 647 |

| Copy | Requests | 201 | 429 | 503 |
| ---: | ---: | ---: | ---: | ---: |
| 1 | 900 | 373 | 274 | 253 |
| 2 | 900 | 374 | 273 | 253 |

## Redis windows

Counted from the decision pre-results Redis keeps per decision ID. Passed means the rate check let the decision through to the daily quota.
Redis runs without persistence, so the windows before the restart were lost with it; PostgreSQL still has those decisions.
Redis was stopped and started inside the same minute window (21:23 UTC), so that minute's counter began again from zero and the minute could pass up to twice the per-minute limit. This is the accepted cost of Redis without persistence (QG-08).
In this run basic passed 60 in that minute before the stop (by PostgreSQL decision time) and 60 after the restart: 120 against a limit of 60.
In this run pro passed 273 in that minute before the stop (by PostgreSQL decision time) and 74 after the restart: 347 against a limit of 1200.

**basic** (limit 60 per minute)

| Window (UTC) | Passed | rate_limited |
| --- | ---: | ---: |
| before the restart (PostgreSQL) | 90 | 213 |
| 21:23 | 60 | 14 |
| 21:24 | 60 | 210 |

**pro** (limit 1200 per minute)

| Window (UTC) | Passed | rate_limited |
| --- | ---: | ---: |
| before the restart (PostgreSQL) | 303 | 0 |
| 21:23 | 74 | 0 |
| 21:24 | 270 | 0 |

## Checks

- PASS every response was 201, a known 429 or 503: 0 other responses
- PASS handler ran once per 201: handler calls 747, 201 responses 747
- PASS before the outage every request got a decision: 600 requests, 0 without a decision
- PASS during the outage every request got 503 and the handler did not run: 420 requests sent +33.9s..+55.0s after the stop, 0 not 503; handler calls 393 then 393
- PASS after recovery every request got a decision again: 624 requests sent from +58.7s, 0 without a decision, 312 allowed
- PASS both copies allowed requests before and after the outage: 201s before: copy 1 195, copy 2 195; after: copy 1 156, copy 2 156
- PASS basic customer: one PostgreSQL record per decided request: records 647, decided responses 647, uncertain 503s 9
- PASS basic customer: PostgreSQL records match responses: allowed/daily/rate records 100/110/437, responses 100/110/437
- PASS basic customer: daily usage equals allowed records: daily usage 100, allowed 100
- PASS basic customer: daily limit kept: allowed 100 of 100; daily_quota records 110
- PASS basic customer: every Redis window within the per-minute limit: 2 windows, most passed in one window 60, limit 60
- PASS basic customer: Redis windows match PostgreSQL (after the Redis restart): Redis passed/limited 120/224, PostgreSQL 120/224, uncertain 503s 9
- PASS basic customer: before the Redis restart, at most the limit per minute: passed 90 in 2 minutes, at most 120
- PASS pro customer: one PostgreSQL record per decided request: records 647, decided responses 647, uncertain 503s 9
- PASS pro customer: PostgreSQL records match responses: allowed/daily/rate records 647/0/0, responses 647/0/0
- PASS pro customer: daily usage equals allowed records: daily usage 647, allowed 647
- PASS pro customer: daily limit kept: allowed 647 of 100000; daily_quota records 0
- PASS pro customer: limits above its whole load, never refused: 900 requests; 429 responses 0, refusal records 0
- PASS pro customer: every Redis window within the per-minute limit: 2 windows, most passed in one window 270, limit 1200
- PASS pro customer: Redis windows match PostgreSQL (after the Redis restart): Redis passed/limited 344/0, PostgreSQL 344/0, uncertain 503s 9
- PASS pro customer: before the Redis restart, at most the limit per minute: passed 303 in 2 minutes, at most 2400
- PASS apps reconnected without a restart: start time and restart count of api, api-2, example-api, example-api-2 unchanged
- PASS no secret in the app logs: admin token, database password and both API keys: 0 found

23 of 23 checks passed.

## Cleanup

`docker compose down -v --remove-orphans --rmi local`, then Docker was asked for anything labelled with the project: removed; no container, volume, network or image of quotagate-qg13 left.

## What this does not show

One machine, one dependency outage at a time, a planned `docker compose stop`. It shows how QuotaGate behaves and recovers in that setting. It is not a production availability, HA or capacity claim.
