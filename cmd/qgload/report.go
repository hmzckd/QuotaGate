package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"quotagate/internal/loadtest"
)

type report struct {
	StartedAt   time.Time           `json:"started_at"`
	FinishedAt  time.Time           `json:"finished_at"`
	Settings    settings            `json:"settings"`
	Environment env                 `json:"environment"`
	Customers   []loadtest.Customer `json:"customers"`
	Baseline    loadtest.Summary    `json:"baseline"`
	QuotaGate   loadtest.Summary    `json:"quotagate"`
	AddedP50    time.Duration       `json:"added_p50"`
	AddedP95    time.Duration       `json:"added_p95"`
	TargetMet   bool                `json:"target_met"`
	PerCustomer []customerRow       `json:"per_customer"`
	Recorded    []loadtest.Recorded `json:"recorded"`
	Checks      []loadtest.Check    `json:"checks"`
}

type env struct {
	Commit    string `json:"commit"`
	GoVersion string `json:"go_version"`
	Platform  string `json:"platform"`
	CPUs      int    `json:"cpus"`
	CPUModel  string `json:"cpu_model"`
	Note      string `json:"note"`
}

type customerRow struct {
	Requests    int `json:"requests"`
	Allowed     int `json:"allowed"`
	DailyQuota  int `json:"daily_quota"`
	RateLimited int `json:"rate_limited"`
	Other       int `json:"other"`
}

func environment(note string) env {
	return env{Commit: commit(), GoVersion: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH,
		CPUs: runtime.NumCPU(), CPUModel: cpuModel(), Note: note}
}

// commit names the code under test; "+dirty" means uncommitted changes.
func commit() string {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	id := strings.TrimSpace(string(out))
	if status, err := exec.Command("git", "status", "--porcelain").Output(); err == nil && len(status) > 0 {
		id += "+dirty"
	}
	return id
}

func cpuModel() string {
	if model := os.Getenv("PROCESSOR_IDENTIFIER"); model != "" { // Windows
		return model
	}
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return "unknown"
	}
	for _, line := range strings.Split(string(data), "\n") {
		if name, value, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(name) == "model name" {
			return strings.TrimSpace(value)
		}
	}
	return "unknown"
}

func perCustomer(customers []loadtest.Customer, results []loadtest.Result) []customerRow {
	rows := make([]customerRow, len(customers))
	for _, r := range results {
		row := &rows[r.Customer]
		row.Requests++
		switch {
		case r.Status == http.StatusCreated:
			row.Allowed++
		case r.Status == http.StatusTooManyRequests && r.Reason == "daily_quota":
			row.DailyQuota++
		case r.Status == http.StatusTooManyRequests && r.Reason == "rate_limited":
			row.RateLimited++
		default:
			row.Other++
		}
	}
	return rows
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
	s, e := r.Settings, r.Environment
	p("# QG-11 load report")
	p("")
	p("- Run: %s to %s UTC", r.StartedAt.Format("2006-01-02 15:04:05"), r.FinishedAt.Format("15:04:05"))
	p("- Code: commit `%s`, %s %s, %d CPUs (%s)", e.Commit, e.GoVersion, e.Platform, e.CPUs, e.CPUModel)
	if e.Note != "" {
		p("- Environment: %s", e.Note)
	}
	p("- Load per target: %g req/s for %s, at most %d in flight, seed %d, %s unmeasured warm-up", s.Rate, s.Duration, s.Concurrency, s.Seed, s.Warmup)
	p("- Target, fixed before the run: p95 latency added by the middleware < %s", s.TargetP95Added)
	p("")
	p("## Latency")
	p("")
	p("Client side, from the scheduled send time to the end of the response. Both runs send the same seeded plan to the same example API handler; the only difference is the QuotaGate middleware.")
	p("")
	p("| | Requests | Achieved rate | p50 | p95 | p99 | Max | Errors |")
	p("| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |")
	row := func(name string, x loadtest.Summary) {
		p("| %s | %d | %.1f/s | %s | %s | %s | %s | %d (%.2f%%) |", name, x.Requests, x.Rate, ms(x.P50), ms(x.P95), ms(x.P99), ms(x.Max), x.Errors, percent(x.Errors, x.Requests))
	}
	row("Baseline, no middleware", r.Baseline)
	row("QuotaGate middleware", r.QuotaGate)
	p("| Added by the middleware | | | %s | %s | %s | | |", ms(r.AddedP50), ms(r.AddedP95), ms(r.QuotaGate.P99-r.Baseline.P99))
	p("")
	p("Target: **%s**. p95 added %s, target < %s.", metWord(r.TargetMet), ms(r.AddedP95), s.TargetP95Added)
	p("")
	p("## Decisions in the QuotaGate run")
	p("")
	p("| Customer | Kind | Daily limit | Per minute | Requests | 201 | 429 daily_quota | 429 rate_limited | Other |")
	p("| ---: | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |")
	for i, c := range r.Customers {
		x := r.PerCustomer[i]
		p("| %d | %s | %d | %d | %d | %d | %d | %d | %d |", i+1, c.Kind, c.DailyLimit, c.RatePerMin, x.Requests, x.Allowed, x.DailyQuota, x.RateLimited, x.Other)
	}
	p("")
	p("Warm-up requests went to open customers only; they are left out of this table and the latency, but included in the checks below.")
	p("")
	p("## Correctness checks")
	p("")
	p("Client responses compared with PostgreSQL decisions and daily usage, and with the example API's handler count.")
	p("")
	for _, c := range r.Checks {
		mark := "PASS"
		if !c.OK {
			mark = "FAIL"
		}
		p("- %s %s: %s", mark, c.Name, c.Detail)
	}
	p("")
	p("## What this does not show")
	p("")
	p("A local run with the load generator, both example APIs, QuotaGate, PostgreSQL and Redis on one machine. It compares the middleware with its own baseline under the same conditions. It is not a production capacity, HA or general p95 SLA figure.")
	return b.String()
}

func ms(d time.Duration) string { return fmt.Sprintf("%.1f ms", float64(d)/float64(time.Millisecond)) }

func percent(part, whole int) float64 {
	if whole == 0 {
		return 0
	}
	return 100 * float64(part) / float64(whole)
}

func metWord(met bool) string {
	if met {
		return "met"
	}
	return "missed"
}
