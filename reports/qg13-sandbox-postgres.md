# QG-13 fault demo: postgres outage

- Run: 2026-10-12 21:24:44 to 21:26:14 UTC
- Code: commit `290044b+dirty`, go1.27.0 linux/amd64, Docker 29.8.2
- Environment: Linux cloud VM, 4 CPUs (Intel Xeon 2.10GHz), Docker 29.8.2 in the same VM, run 1 of 2
- Compose project `quotagate-qg13` from compose.yaml, compose.qg08.yaml, compose.qg13.yaml and the local override qg13-sandbox-build.yaml: PostgreSQL, Redis, two QuotaGate copies (`serve-combined`), one example API in front of each copy
- Load: 20 req/s for 1m30s, open loop, half to each example API, at most 32 in flight per copy; the two customers take turns
- Outage: `docker compose stop db` at +30.0s, `docker compose up` at +55.0s

| Customer | Daily limit | Per minute |
| --- | ---: | ---: |
| basic | 100 | 60 |
| pro | 100000 | 1200 |

## Timeline

Seconds after the load started.

| Time | Event |
| ---: | --- |
| +0.0s | load starts |
| +30.0s | `docker compose stop db` |
| +30.2s | copy 2 `/health/ready` = 503 |
| +30.2s | copy 1 `/health/ready` = 503 |
| +30.4s | db stopped |
| +33.4s | in-flight requests drained; handler count 402 |
| +55.0s | handler count 402; `docker compose up --wait db` |
| +55.6s | copy 2 `/health/ready` = 200 |
| +55.6s | copy 1 `/health/ready` = 200 |
| +59.0s | `up --wait` returned: db healthy |
| +89.9s | load ends |

## Responses every 5 seconds

By the time the request was sent.

| Sent | Requests | 201 | 429 | 503 | Other |
| ---: | ---: | ---: | ---: | ---: | ---: |
| +0s | 100 | 100 | 0 | 0 | 0 |
| +5s | 100 | 60 | 40 | 0 | 0 |
| +10s | 100 | 50 | 50 | 0 | 0 |
| +15s | 100 | 90 | 10 | 0 | 0 |
| +20s | 100 | 50 | 50 | 0 | 0 |
| +25s | 100 | 50 | 50 | 0 | 0 |
| +30s | 100 | 2 | 2 | 96 | 0 |
| +35s | 100 | 0 | 0 | 100 | 0 |
| +40s | 100 | 0 | 0 | 100 | 0 |
| +45s | 100 | 0 | 0 | 100 | 0 |
| +50s | 100 | 12 | 13 | 75 | 0 |
| +55s | 100 | 50 | 50 | 0 | 0 |
| +60s | 100 | 50 | 50 | 0 | 0 |
| +65s | 100 | 50 | 50 | 0 | 0 |
| +70s | 100 | 50 | 50 | 0 | 0 |
| +75s | 100 | 50 | 50 | 0 | 0 |
| +80s | 100 | 50 | 50 | 0 | 0 |
| +85s | 100 | 50 | 50 | 0 | 0 |

## Decisions per customer

Client responses, then what PostgreSQL recorded. An uncertain 503 was in flight while db went down or came back.

| Customer | Requests | 201 | 429 daily_quota | 429 rate_limited | 503 (uncertain) | Records | Allowed | daily_quota | rate_limited | daily_usage |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| basic | 900 | 100 | 80 | 485 | 235 (9) | 665 | 100 | 80 | 485 | 100 |
| pro | 900 | 664 | 0 | 0 | 236 (10) | 664 | 664 | 0 | 0 | 664 |

| Copy | Requests | 201 | 429 | 503 |
| ---: | ---: | ---: | ---: | ---: |
| 1 | 900 | 382 | 282 | 236 |
| 2 | 900 | 382 | 283 | 235 |

## Redis windows

Counted from the decision pre-results Redis keeps per decision ID. Passed means the rate check let the decision through to the daily quota.

**basic** (limit 60 per minute)

| Window (UTC) | Passed | rate_limited |
| --- | ---: | ---: |
| 21:24 | 60 | 100 |
| 21:25 | 60 | 305 |
| 21:26 | 60 | 80 |

**pro** (limit 1200 per minute)

| Window (UTC) | Passed | rate_limited |
| --- | ---: | ---: |
| 21:24 | 160 | 0 |
| 21:25 | 364 | 0 |
| 21:26 | 140 | 0 |

## Checks

- PASS every response was 201, a known 429 or 503: 0 other responses
- PASS handler ran once per 201: handler calls 764, 201 responses 764
- PASS before the outage every request got a decision: 600 requests, 0 without a decision
- PASS during the outage every request got 503 and the handler did not run: 392 requests sent +33.4s..+55.0s after the stop, 0 not 503; handler calls 402 then 402
- PASS after recovery every request got a decision again: 626 requests sent from +58.6s, 0 without a decision, 313 allowed
- PASS both copies allowed requests before and after the outage: 201s before: copy 1 200, copy 2 200; after: copy 1 157, copy 2 156
- PASS basic customer: one PostgreSQL record per decided request: records 665, decided responses 665, uncertain 503s 9
- PASS basic customer: PostgreSQL records match responses: allowed/daily/rate records 100/80/485, responses 100/80/485
- PASS basic customer: daily usage equals allowed records: daily usage 100, allowed 100
- PASS basic customer: daily limit kept: allowed 100 of 100; daily_quota records 80
- PASS basic customer: every Redis window within the per-minute limit: 3 windows, most passed in one window 60, limit 60
- PASS basic customer: Redis windows match PostgreSQL (whole run): Redis passed/limited 180/485, PostgreSQL 180/485, uncertain 503s 9
- PASS pro customer: one PostgreSQL record per decided request: records 664, decided responses 664, uncertain 503s 10
- PASS pro customer: PostgreSQL records match responses: allowed/daily/rate records 664/0/0, responses 664/0/0
- PASS pro customer: daily usage equals allowed records: daily usage 664, allowed 664
- PASS pro customer: daily limit kept: allowed 664 of 100000; daily_quota records 0
- PASS pro customer: limits above its whole load, never refused: 900 requests; 429 responses 0, refusal records 0
- PASS pro customer: every Redis window within the per-minute limit: 3 windows, most passed in one window 364, limit 1200
- PASS pro customer: Redis windows match PostgreSQL (whole run): Redis passed/limited 664/0, PostgreSQL 664/0, uncertain 503s 10
- PASS apps reconnected without a restart: start time and restart count of api, api-2, example-api, example-api-2 unchanged
- PASS no secret in the app logs: admin token, database password and both API keys: 0 found

21 of 21 checks passed.

## Cleanup

`docker compose down -v --remove-orphans --rmi local`, then Docker was asked for anything labelled with the project: removed; no container, volume, network or image of quotagate-qg13 left.

## What this does not show

One machine, one dependency outage at a time, a planned `docker compose stop`. It shows how QuotaGate behaves and recovers in that setting. It is not a production availability, HA or capacity claim.
