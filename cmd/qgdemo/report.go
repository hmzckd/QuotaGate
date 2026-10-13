package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

type report struct {
	StartedAt   time.Time    `json:"started_at"`
	FinishedAt  time.Time    `json:"finished_at"`
	Settings    settings     `json:"settings"`
	Environment env          `json:"environment"`
	Customers   []customer   `json:"customers"`
	Timeline    timeline     `json:"timeline"`
	Readiness   []change     `json:"readiness"`
	Handlers    handlerCalls `json:"handler_calls"`
	Tallies     []tally      `json:"responses"`
	Copies      []copyRow    `json:"copies"`
	Buckets     []bucket     `json:"buckets"`
	Recorded    []recorded   `json:"postgres"`
	Windows     [][]window   `json:"redis_windows"`
	Checks      []check      `json:"checks"`
	Cleanup     string       `json:"cleanup"`
	Error       string       `json:"error,omitempty"`
}

type env struct {
	Commit    string `json:"commit"`
	GoVersion string `json:"go_version"`
	Platform  string `json:"platform"`
	Docker    string `json:"docker"`
	Note      string `json:"note"`
}

type copyRow struct {
	Requests    int `json:"requests"`
	Created     int `json:"created"`
	Refused     int `json:"refused"`
	Unavailable int `json:"unavailable"`
	Other       int `json:"other"`
}

// bucket counts the responses to requests sent in one 5-second slice.
type bucket struct {
	From int `json:"from_second"`
	copyRow
}

func environment(note string) env {
	e := env{Commit: "unknown", GoVersion: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH, Docker: "unknown", Note: note}
	if out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output(); err == nil {
		e.Commit = strings.TrimSpace(string(out))
		// "+dirty" means the code under test had uncommitted changes.
		if status, err := exec.Command("git", "status", "--porcelain").Output(); err == nil && len(status) > 0 {
			e.Commit += "+dirty"
		}
	}
	if out, err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").Output(); err == nil {
		e.Docker = strings.TrimSpace(string(out))
	}
	return e
}

func (row *copyRow) add(status int) {
	row.Requests++
	switch status {
	case http.StatusCreated:
		row.Created++
	case http.StatusTooManyRequests:
		row.Refused++
	case http.StatusServiceUnavailable:
		row.Unavailable++
	default:
		row.Other++
	}
}

func copyRows(requests []request) []copyRow {
	rows := make([]copyRow, 2)
	for _, r := range requests {
		rows[r.Copy].add(r.Status)
	}
	return rows
}

func buckets(e evidence) []bucket {
	var out []bucket
	for _, r := range e.Requests {
		i := int(r.Sent.Sub(e.Timeline.LoadStart) / (5 * time.Second))
		for len(out) <= i {
			out = append(out, bucket{From: 5 * len(out)})
		}
		out[i].add(r.Status)
	}
	return out
}

func (r report) write(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(strings.TrimSuffix(path, filepath.Ext(path))+".json", append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(r.markdown()), 0o644)
}

func (r report) markdown() string {
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	s, e, t := r.Settings, r.Environment, r.Timeline
	name := service[s.Outage]
	at := func(x time.Time) string { return since(t.LoadStart, x) }
	files := "compose.yaml, compose.qg08.yaml, compose.qg13.yaml"
	if s.Override != "" {
		files += " and the local override " + s.Override
	}

	p("# QG-13 fault demo: %s outage", s.Outage)
	p("")
	p("- Run: %s to %s UTC", r.StartedAt.Format("2006-01-02 15:04:05"), r.FinishedAt.Format("15:04:05"))
	p("- Code: commit `%s`, %s %s, Docker %s", e.Commit, e.GoVersion, e.Platform, e.Docker)
	if e.Note != "" {
		p("- Environment: %s", e.Note)
	}
	p("- Compose project `%s` from %s: PostgreSQL, Redis, two QuotaGate copies (`serve-combined`), one example API in front of each copy", project, files)
	p("- Load: %g req/s for %s, open loop, half to each example API, at most %d in flight per copy; the two customers take turns", s.Rate, s.total(), s.Concurrency)
	p("- Outage: `docker compose stop %s` at %s, `docker compose up` at %s", name, at(t.StopIssued), at(t.StartIssued))
	if r.Error != "" {
		p("- **Run stopped with an error:** %s", r.Error)
	}
	p("")
	p("| Customer | Daily limit | Per minute |")
	p("| --- | ---: | ---: |")
	for _, c := range r.Customers {
		p("| %s | %d | %d |", c.Name, c.DailyLimit, c.RatePerMin)
	}

	p("")
	p("## Timeline")
	p("")
	p("Seconds after the load started.")
	p("")
	p("| Time | Event |")
	p("| ---: | --- |")
	type event struct {
		at   time.Time
		text string
	}
	events := []event{
		{t.LoadStart, "load starts"},
		{t.StopIssued, "`docker compose stop " + name + "`"},
		{t.Stopped, name + " stopped"},
		{t.QuietFrom, fmt.Sprintf("in-flight requests drained; handler count %d", r.Handlers.QuietFrom)},
		{t.QuietTo, fmt.Sprintf("handler count %d; `docker compose up --wait %s`", r.Handlers.QuietTo, name)},
		{t.Started, "`up --wait` returned: " + name + " healthy"},
		{t.LoadEnd, "load ends"},
	}
	for _, c := range r.Readiness {
		if c.At.Before(t.LoadStart) {
			continue
		}
		events = append(events, event{c.At, fmt.Sprintf("copy %d `/health/ready` = %d", c.Copy, c.Status)})
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].at.Before(events[j].at) })
	for _, ev := range events {
		if !ev.at.IsZero() {
			p("| %s | %s |", at(ev.at), ev.text)
		}
	}
	for _, c := range r.Readiness {
		if c.Status == 0 {
			p("")
			p("`/health/ready` = 0: no answer within the watcher's 2s timeout.")
			break
		}
	}

	p("")
	p("## Responses every 5 seconds")
	p("")
	p("By the time the request was sent.")
	p("")
	p("| Sent | Requests | 201 | 429 | 503 | Other |")
	p("| ---: | ---: | ---: | ---: | ---: | ---: |")
	for _, x := range r.Buckets {
		p("| +%ds | %d | %d | %d | %d | %d |", x.From, x.Requests, x.Created, x.Refused, x.Unavailable, x.Other)
	}

	p("")
	p("## Decisions per customer")
	p("")
	p("Client responses, then what PostgreSQL recorded. An uncertain 503 was in flight while %s went down or came back.", name)
	p("")
	p("| Customer | Requests | 201 | 429 daily_quota | 429 rate_limited | 503 (uncertain) | Records | Allowed | daily_quota | rate_limited | daily_usage |")
	p("| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |")
	for i, c := range r.Customers {
		if i >= len(r.Tallies) || i >= len(r.Recorded) {
			break
		}
		x, db := r.Tallies[i], r.Recorded[i]
		p("| %s | %d | %d | %d | %d | %d (%d) | %d | %d | %d | %d | %d |", c.Name, x.Requests, x.Created, x.DailyQuota, x.RateLimited,
			x.Unavailable, x.Uncertain, db.Decisions, db.Allowed, db.DailyQuota, db.RateLimited, db.DailyUsed)
	}
	p("")
	p("| Copy | Requests | 201 | 429 | 503 |")
	p("| ---: | ---: | ---: | ---: | ---: |")
	for i, x := range r.Copies {
		p("| %d | %d | %d | %d | %d |", i+1, x.Requests, x.Created, x.Refused, x.Unavailable)
	}

	p("")
	p("## Redis windows")
	p("")
	p("Counted from the decision pre-results Redis keeps per decision ID. Passed means the rate check let the decision through to the daily quota.")
	if s.Outage == "redis" {
		p("Redis runs without persistence, so the windows before the restart were lost with it; PostgreSQL still has those decisions.")
		if minute(t.StopIssued) == minute(t.Started) {
			p("Redis was stopped and started inside the same minute window (%s UTC), so that minute's counter began again from zero and the minute could pass up to twice the per-minute limit. This is the accepted cost of Redis without persistence (QG-08).", t.StopIssued.UTC().Format("15:04"))
			for i, c := range r.Customers {
				if i < len(r.Recorded) && i < len(r.Windows) && len(r.Windows[i]) > 0 && minute(r.Windows[i][0].Start) == minute(t.StopIssued) {
					p("In this run %s passed %d in that minute before the stop (by PostgreSQL decision time) and %d after the restart: %d against a limit of %d.",
						c.Name, r.Recorded[i].PassedStopMinute, r.Windows[i][0].Passed, r.Recorded[i].PassedStopMinute+r.Windows[i][0].Passed, c.RatePerMin)
				}
			}
		}
	}
	for i, c := range r.Customers {
		if i >= len(r.Windows) {
			break
		}
		p("")
		p("**%s** (limit %d per minute)", c.Name, c.RatePerMin)
		p("")
		p("| Window (UTC) | Passed | rate_limited |")
		p("| --- | ---: | ---: |")
		if s.Outage == "redis" && i < len(r.Recorded) {
			p("| before the restart (PostgreSQL) | %d | %d |", r.Recorded[i].PassedBefore, r.Recorded[i].LimitedBefore)
		}
		for _, w := range r.Windows[i] {
			p("| %s | %d | %d |", w.Start.Format("15:04"), w.Passed, w.Limited)
		}
	}

	p("")
	p("## Checks")
	p("")
	passed := 0
	for _, c := range r.Checks {
		mark := "FAIL"
		if c.OK {
			mark = "PASS"
			passed++
		}
		p("- %s %s: %s", mark, c.Name, c.Detail)
	}
	p("")
	p("%d of %d checks passed.", passed, len(r.Checks))
	p("")
	p("## Cleanup")
	p("")
	p("`docker compose down -v --remove-orphans --rmi local`, then Docker was asked for anything labelled with the project: %s.", r.Cleanup)
	p("")
	p("## What this does not show")
	p("")
	p("One machine, one dependency outage at a time, a planned `docker compose stop`. It shows how QuotaGate behaves and recovers in that setting. It is not a production availability, HA or capacity claim.")
	return b.String()
}
