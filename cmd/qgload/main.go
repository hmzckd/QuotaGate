// Command qgload runs the QG-11 load comparison: the same seeded load against
// the example API without middleware (baseline) and with the QuotaGate
// middleware, then reconciles the QuotaGate run with PostgreSQL.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"quotagate/internal/loadtest"
)

type settings struct {
	QuotaGateURL   string        `json:"quotagate_url"`
	GuardedURL     string        `json:"guarded_url"`
	BaselineURL    string        `json:"baseline_url"`
	Rate           float64       `json:"rate"`
	Duration       time.Duration `json:"duration"`
	Concurrency    int           `json:"concurrency"`
	Seed           uint64        `json:"seed"`
	Warmup         time.Duration `json:"warmup"`
	TargetP95Added time.Duration `json:"target_p95_added"`
	Note           string        `json:"note"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "qgload:", err)
		os.Exit(1)
	}
}

func run() error {
	var s settings
	var reportPath, composeEnv string
	var keep bool
	flag.StringVar(&s.QuotaGateURL, "quotagate", "http://127.0.0.1:18080", "QuotaGate origin, for the admin API and readiness")
	flag.StringVar(&s.GuardedURL, "guarded", "http://127.0.0.1:18081", "example API origin with the QuotaGate middleware")
	flag.StringVar(&s.BaselineURL, "baseline", "http://127.0.0.1:18083", "example API origin started with QUOTAGATE_MIDDLEWARE=off")
	flag.Float64Var(&s.Rate, "rate", 50, "requests per second")
	flag.DurationVar(&s.Duration, "duration", 5*time.Minute, "measured time per target")
	flag.IntVar(&s.Concurrency, "concurrency", 20, "most requests in flight at once")
	flag.Uint64Var(&s.Seed, "seed", 11, "seed for the request plan; the same seed sends the same load")
	flag.DurationVar(&s.Warmup, "warmup", 10*time.Second, "unmeasured warm-up per target, open customers only")
	flag.DurationVar(&s.TargetP95Added, "target-p95-added", 30*time.Millisecond, "target for p95 latency added by the middleware")
	flag.StringVar(&s.Note, "note", "", "free text about the environment, stored in the report")
	flag.StringVar(&reportPath, "report", "", "Markdown report path; a .json file is written next to it (default reports/qg11-<UTC time>.md)")
	flag.StringVar(&composeEnv, "compose-env", "", "read the admin token and database password from this .env.compose instead of ADMIN_TOKEN and DATABASE_URL")
	flag.BoolVar(&keep, "keep", false, "keep the test customers and their decisions after the run")
	flag.Parse()
	if flag.NArg() > 0 || s.Rate <= 0 || s.Duration <= 0 || s.Concurrency < 1 || s.Warmup < 0 {
		flag.Usage()
		return errors.New("invalid arguments")
	}
	planned := int(s.Rate * s.Duration.Seconds())
	warmups := int(s.Rate * s.Warmup.Seconds())
	if planned < 1 {
		return errors.New("rate × duration must be at least one request")
	}
	if reportPath == "" {
		reportPath = "reports/qg11-" + time.Now().UTC().Format("20060102-150405") + ".md"
	}
	adminToken, databaseURL, err := secrets(composeEnv)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("database pool: %w", err)
	}
	defer pool.Close()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = s.Concurrency
	httpClient := &http.Client{Transport: transport, Timeout: 10 * time.Second}

	if err := preflight(ctx, httpClient, pool, s); err != nil {
		return err
	}
	customers := loadtest.DefaultCustomers()
	runID := fmt.Sprintf("qgload-%d", time.Now().Unix())
	ids, keys, err := createCustomers(ctx, httpClient, s.QuotaGateURL, adminToken, runID, customers)
	if !keep {
		defer cleanup(pool, ids, runID)
	}
	if err != nil {
		return err
	}

	r := report{Settings: s, Environment: environment(s.Note), Customers: customers, StartedAt: time.Now().UTC()}
	plan := loadtest.Plan(customers, planned, s.Seed)
	warm := loadtest.WarmupPlan(customers, warmups)
	fmt.Fprintf(os.Stderr, "target before measuring: p95 added latency < %s\n", s.TargetP95Added)

	baseline := loadtest.Target{URL: s.BaselineURL + "/jobs", Keys: keys}
	fmt.Fprintf(os.Stderr, "baseline: %d warm-up + %d requests at %g/s\n", warmups, planned, s.Rate)
	if _, err := loadtest.Run(ctx, httpClient, baseline, warm, s.Rate, s.Concurrency); err != nil {
		return err
	}
	baseResults, err := loadtest.Run(ctx, httpClient, baseline, plan, s.Rate, s.Concurrency)
	if err != nil {
		return err
	}

	guarded := loadtest.Target{URL: s.GuardedURL + "/jobs", Keys: keys}
	before, err := handlerCalls(ctx, httpClient, s.GuardedURL)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "QuotaGate: %d warm-up + %d requests at %g/s\n", warmups, planned, s.Rate)
	warmResults, err := loadtest.Run(ctx, httpClient, guarded, warm, s.Rate, s.Concurrency)
	if err != nil {
		return err
	}
	guardResults, err := loadtest.Run(ctx, httpClient, guarded, plan, s.Rate, s.Concurrency)
	if err != nil {
		return err
	}
	after, err := handlerCalls(ctx, httpClient, s.GuardedURL)
	if err != nil {
		return err
	}
	if r.Recorded, err = recorded(ctx, pool, ids); err != nil {
		return err
	}

	// Warm-up decisions are real decisions too, so they are reconciled; only
	// the latency summary leaves them out.
	r.Checks = loadtest.Reconcile(customers, append(warmResults, guardResults...), r.Recorded, after-before)
	r.Baseline, r.QuotaGate = loadtest.Summarize(baseResults), loadtest.Summarize(guardResults)
	r.PerCustomer = perCustomer(customers, guardResults)
	r.AddedP50, r.AddedP95 = r.QuotaGate.P50-r.Baseline.P50, r.QuotaGate.P95-r.Baseline.P95
	r.TargetMet = r.AddedP95 < s.TargetP95Added
	r.FinishedAt = time.Now().UTC()
	if err := r.write(reportPath); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "p95 baseline %s, QuotaGate %s, added %s (target < %s: %s)\n",
		ms(r.Baseline.P95), ms(r.QuotaGate.P95), ms(r.AddedP95), s.TargetP95Added, metWord(r.TargetMet))
	failed := 0
	for _, c := range r.Checks {
		if !c.OK {
			failed++
			fmt.Fprintf(os.Stderr, "FAILED %s: %s\n", c.Name, c.Detail)
		}
	}
	fmt.Fprintf(os.Stderr, "report: %s\n", reportPath)
	if failed > 0 {
		return fmt.Errorf("%d correctness checks failed", failed)
	}
	return nil
}

// secrets come from the environment or from .env.compose, never from flags,
// so they stay out of shell history and process lists.
func secrets(composeEnv string) (adminToken, databaseURL string, err error) {
	if composeEnv == "" {
		adminToken, databaseURL = os.Getenv("ADMIN_TOKEN"), os.Getenv("DATABASE_URL")
		if adminToken == "" || databaseURL == "" {
			return "", "", errors.New("set ADMIN_TOKEN and DATABASE_URL, or pass -compose-env .env.compose")
		}
		return adminToken, databaseURL, nil
	}
	values, err := readEnvFile(composeEnv)
	if err != nil {
		return "", "", err
	}
	port := values["QG_DB_PORT"]
	if port == "" {
		port = "55432"
	}
	if values["QG_ADMIN_TOKEN"] == "" || values["QG_DB_PASSWORD"] == "" {
		return "", "", errors.New("compose env file needs QG_ADMIN_TOKEN and QG_DB_PASSWORD")
	}
	u := url.URL{Scheme: "postgres", User: url.UserPassword("quotagate", values["QG_DB_PASSWORD"]),
		Host: "127.0.0.1:" + port, Path: "/quotagate", RawQuery: "sslmode=disable"}
	return values["QG_ADMIN_TOKEN"], u.String(), nil
}

func readEnvFile(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		values[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return values, scanner.Err()
}

// preflight refuses to measure a wrong setup: a swapped baseline, a service
// that is not ready, or a run that would cross UTC midnight and reset the
// daily quota halfway.
func preflight(ctx context.Context, client *http.Client, pool *pgxpool.Pool, s settings) error {
	checks := []struct {
		method, url, why string
		want             int
	}{
		{"GET", s.QuotaGateURL + "/health/ready", "QuotaGate is not ready", 200},
		{"GET", s.GuardedURL + "/health/live", "guarded example API is not running", 200},
		{"GET", s.BaselineURL + "/health/live", "baseline example API is not running", 200},
		{"POST", s.GuardedURL + "/jobs", "guarded example API accepted a request without a key; is the middleware on?", 401},
		{"POST", s.BaselineURL + "/jobs", "baseline example API refused a request; is QUOTAGATE_MIDDLEWARE=off?", 201},
	}
	for _, c := range checks {
		status, err := call(ctx, client, c.method, c.url, "", nil, nil)
		if err != nil || status != c.want {
			return fmt.Errorf("preflight: %s (status %d)", c.why, status)
		}
	}
	var untilMidnight float64
	if err := pool.QueryRow(ctx, `SELECT extract(epoch FROM date_trunc('day', now() AT TIME ZONE 'UTC') + interval '1 day' - now() AT TIME ZONE 'UTC')::float8`).Scan(&untilMidnight); err != nil {
		return fmt.Errorf("read database clock: %w", err)
	}
	if need := 2*(s.Warmup+s.Duration) + 2*time.Minute; time.Duration(untilMidnight*float64(time.Second)) < need {
		return fmt.Errorf("the run needs about %s and would cross UTC midnight; start after midnight", need)
	}
	return nil
}

func createCustomers(ctx context.Context, client *http.Client, base, adminToken, runID string, customers []loadtest.Customer) (ids, keys []string, err error) {
	for i, customer := range customers {
		var created struct {
			ID     string `json:"id"`
			APIKey string `json:"api_key"`
		}
		name := fmt.Sprintf("%s %s %d", runID, customer.Kind, i+1)
		status, err := call(ctx, client, "POST", base+"/v1/admin/customers", adminToken, map[string]string{"name": name}, &created)
		if err != nil || status != http.StatusCreated {
			return ids, keys, fmt.Errorf("create test customer: status %d", status)
		}
		ids, keys = append(ids, created.ID), append(keys, created.APIKey)
		policy := map[string]any{"daily_limit": customer.DailyLimit, "rate_limit_per_minute": customer.RatePerMin}
		status, err = call(ctx, client, "PUT", base+"/v1/admin/customers/"+created.ID+"/policies/job.create", adminToken, policy, nil)
		if err != nil || status != http.StatusOK {
			return ids, keys, fmt.Errorf("set test policy: status %d", status)
		}
	}
	return ids, keys, nil
}

func call(ctx context.Context, client *http.Client, method, url, token string, body, out any) (int, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return 0, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if out != nil {
		err = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(out)
	}
	return resp.StatusCode, err
}

func handlerCalls(ctx context.Context, client *http.Client, base string) (int64, error) {
	var stats struct {
		HandlerCalls int64 `json:"handler_calls"`
	}
	if status, err := call(ctx, client, "GET", base+"/demo/stats", "", nil, &stats); err != nil || status != http.StatusOK {
		return 0, fmt.Errorf("read example API handler count: status %d", status)
	}
	return stats.HandlerCalls, nil
}

func recorded(ctx context.Context, pool *pgxpool.Pool, ids []string) ([]loadtest.Recorded, error) {
	out := make([]loadtest.Recorded, len(ids))
	for i, id := range ids {
		err := pool.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM quotagate.decisions WHERE customer_id = $1),
			(SELECT count(*) FROM quotagate.decisions WHERE customer_id = $1 AND allowed),
			(SELECT count(*) FROM quotagate.decisions WHERE customer_id = $1 AND reason = 'daily_quota'),
			(SELECT count(*) FROM quotagate.decisions WHERE customer_id = $1 AND reason = 'rate_limited'),
			(SELECT coalesce(sum(used), 0)::bigint FROM quotagate.daily_usage WHERE customer_id = $1)`, id).
			Scan(&out[i].Decisions, &out[i].Allowed, &out[i].DailyQuota, &out[i].RateLimited, &out[i].DailyUsed)
		if err != nil {
			return nil, fmt.Errorf("read recorded decisions: %w", err)
		}
	}
	return out, nil
}

// cleanup deletes only the customers this run created; their policies,
// decisions and daily usage go with them (ON DELETE CASCADE). It runs even
// after Ctrl+C, with its own deadline.
func cleanup(pool *pgxpool.Pool, ids []string, runID string) {
	if len(ids) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `DELETE FROM quotagate.customers WHERE id = ANY($1)`, ids); err != nil {
		fmt.Fprintf(os.Stderr, "cleanup failed; delete customers named %q by hand\n", runID+" *")
	}
}
