// Command qgdemo runs the QG-13 end-to-end fault demo in its own Compose
// project: two customers, two QuotaGate copies, steady load, one dependency
// outage and its recovery. It reconciles every response with the example
// APIs' handler counts, PostgreSQL and Redis, writes a report and removes the
// project with its volume.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"quotagate/internal/envfile"
	"quotagate/internal/loadtest"
)

type settings struct {
	Outage      string        `json:"outage"`
	Rate        float64       `json:"rate"`
	Before      time.Duration `json:"before"`
	OutageFor   time.Duration `json:"outage_for"`
	After       time.Duration `json:"after"`
	Concurrency int           `json:"concurrency"`
	Override    string        `json:"override,omitempty"`
	Note        string        `json:"note"`
}

func (s settings) total() time.Duration { return s.Before + s.OutageFor + s.After }

// service is the Compose service stopped for each -outage value.
var service = map[string]string{"redis": "redis", "postgres": "db"}

var (
	quotagateURLs = [2]string{"http://127.0.0.1:" + ports["QG_HTTP_PORT"], "http://127.0.0.1:" + ports["QG_HTTP_2_PORT"]}
	exampleURLs   = [2]string{"http://127.0.0.1:" + ports["QG_EXAMPLE_PORT"], "http://127.0.0.1:" + ports["QG_EXAMPLE_2_PORT"]}
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "qgdemo:", err)
		os.Exit(1)
	}
}

func run() error {
	var s settings
	var envFile, override, reportPath string
	var keep bool
	flag.StringVar(&s.Outage, "outage", "redis", "dependency to stop during the load: redis or postgres")
	flag.Float64Var(&s.Rate, "rate", 20, "requests per second, split evenly over the two copies")
	flag.DurationVar(&s.Before, "before", 30*time.Second, "load before the outage")
	flag.DurationVar(&s.OutageFor, "outage-for", 25*time.Second, "how long the dependency stays stopped")
	flag.DurationVar(&s.After, "after", 35*time.Second, "load after the dependency is started again")
	flag.IntVar(&s.Concurrency, "concurrency", 32, "most requests in flight per copy")
	flag.StringVar(&override, "override", "", "extra Compose file applied last, for example a local build setting")
	flag.StringVar(&s.Note, "note", "", "free text about the environment, stored in the report")
	flag.StringVar(&envFile, "compose-env", ".env.compose", "Compose env file with QG_DB_PASSWORD and QG_ADMIN_TOKEN")
	flag.StringVar(&reportPath, "report", "", "Markdown report path; a .json file is written next to it (default reports/qg13-<outage>-<UTC time>.md)")
	flag.BoolVar(&keep, "keep", false, "leave the demo project running for inspection")
	flag.Parse()
	// The outage needs time to be noticed and drained; recovery needs time
	// for the restart, the drain and some decided requests.
	if flag.NArg() > 0 || service[s.Outage] == "" || s.Rate < 2 || s.Concurrency < 1 ||
		s.Before < 10*time.Second || s.OutageFor < 10*time.Second || s.After < 15*time.Second {
		flag.Usage()
		return errors.New("invalid arguments")
	}
	if override != "" {
		s.Override = filepath.Base(override) // the report names the file, not where it lives
	}
	if reportPath == "" {
		reportPath = "reports/qg13-" + s.Outage + "-" + time.Now().UTC().Format("20060102-150405") + ".md"
	}
	values, err := envfile.Read(envFile)
	if err != nil {
		return err
	}
	adminToken, dbPassword := values["QG_ADMIN_TOKEN"], values["QG_DB_PASSWORD"]
	if adminToken == "" || dbPassword == "" {
		return errors.New("compose env file needs QG_ADMIN_TOKEN and QG_DB_PASSWORD")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dc := newCompose(envFile, override)
	// Never reuse or remove something this run did not create.
	found, err := leftovers(ctx)
	if err != nil {
		return err
	}
	if len(found) > 0 {
		return fmt.Errorf("project %s already exists (%s); remove it first with:\n  %s", project, strings.Join(found, ", "), dc.downCommand())
	}

	r := &report{Settings: s, Environment: environment(s.Note), Customers: demoCustomers}
	demoErr := demo(ctx, s, dc, adminToken, dbPassword, r)
	if ctx.Err() != nil {
		demoErr = errors.New("interrupted")
	}
	// From here a second Ctrl+C stops the program at once; the next run then
	// refuses to start and prints the command that removes the project.
	stop()
	if demoErr != nil {
		r.Error = demoErr.Error()
		if r.FinishedAt.IsZero() {
			r.FinishedAt = time.Now().UTC()
		}
	}
	var cleanupErr error
	if keep {
		r.Cleanup = "kept with -keep"
		fmt.Fprintf(os.Stderr, "project kept; remove it with:\n  %s\n", dc.downCommand())
	} else {
		fmt.Fprintf(os.Stderr, "removing Compose project %s\n", project)
		r.Cleanup, cleanupErr = cleanup(dc)
	}
	if !r.StartedAt.IsZero() {
		if err := r.write(reportPath); err != nil {
			return errors.Join(demoErr, cleanupErr, err)
		}
		fmt.Fprintf(os.Stderr, "report: %s\n", reportPath)
	}
	failed := 0
	for _, c := range r.Checks {
		if !c.OK {
			failed++
			fmt.Fprintf(os.Stderr, "FAILED %s: %s\n", c.Name, c.Detail)
		}
	}
	if failed > 0 {
		demoErr = errors.Join(demoErr, fmt.Errorf("%d of %d checks failed", failed, len(r.Checks)))
	} else if demoErr == nil {
		fmt.Fprintf(os.Stderr, "all %d checks passed\n", len(r.Checks))
	}
	return errors.Join(demoErr, cleanupErr)
}

func demo(ctx context.Context, s settings, dc compose, adminToken, dbPassword string, r *report) error {
	fmt.Fprintf(os.Stderr, "starting Compose project %s (the first build takes a few minutes)\n", project)
	if _, err := dc.run(ctx, append([]string{"up", "-d", "--build", "--wait"}, apps...)...); err != nil {
		return err
	}
	db, err := pgxpool.New(ctx, databaseURL(dbPassword))
	if err != nil {
		return fmt.Errorf("database pool: %w", err)
	}
	defer db.Close()
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:" + ports["QG_REDIS_PORT"]})
	defer rdb.Close()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = s.Concurrency
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}

	if err := preflight(ctx, client, db, s.total()); err != nil {
		return err
	}
	ids, keys, err := createCustomers(ctx, client, adminToken)
	if err != nil {
		return err
	}
	startsBefore, err := dc.starts(ctx)
	if err != nil {
		return err
	}

	w := newWatcher(quotagateURLs)
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	go w.run(watchCtx)
	if _, err := w.waitBoth(ctx, http.StatusOK, 10*time.Second); err != nil {
		return err
	}
	if r.Handlers.Before, err = readHandlerCalls(ctx, client); err != nil {
		return err
	}

	// Each copy's example API gets half the rate. The two customers take
	// turns, and at the same moment the other copy serves the other customer.
	perCopy := int(s.Rate / 2 * s.total().Seconds())
	loadCtx, stopLoad := context.WithCancel(ctx)
	defer stopLoad()
	var results [2][]loadtest.Result
	var loadErr [2]error
	var wg sync.WaitGroup
	t := &r.Timeline
	r.StartedAt = time.Now().UTC()
	t.LoadStart = time.Now()
	fmt.Fprintf(os.Stderr, "load: %g req/s for %s, %s outage from %s\n", s.Rate, s.total(), s.Outage, since(t.LoadStart, t.LoadStart.Add(s.Before)))
	for c := range 2 {
		plan := make([]int, perCopy)
		for i := range plan {
			plan[i] = (i + c) % 2
		}
		target := loadtest.Target{URL: exampleURLs[c] + "/jobs", Keys: keys}
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[c], loadErr[c] = loadtest.Run(loadCtx, client, target, plan, s.Rate/2, s.Concurrency)
		}()
	}
	outageErr := outage(ctx, s, dc, w, client, t, &r.Handlers)
	if outageErr != nil {
		stopLoad()
	}
	wg.Wait()
	t.LoadEnd = time.Now()
	stopWatch()
	r.Readiness = w.history()
	if err := errors.Join(outageErr, loadErr[0], loadErr[1]); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "load finished; reconciling")

	if r.Handlers.After, err = readHandlerCalls(ctx, client); err != nil {
		return err
	}
	startsAfter, err := dc.starts(ctx)
	if err != nil {
		return err
	}
	leaked, err := dc.secretsInLogs(ctx, adminToken, dbPassword, keys[0], keys[1])
	if err != nil {
		return err
	}
	// No decision can be made during a Redis outage, so its middle splits
	// PostgreSQL's decisions into the first Redis's and the restarted one's.
	var split, stopMinute time.Time
	if s.Outage == "redis" {
		split = t.Stopped.Add(t.StartIssued.Sub(t.Stopped) / 2)
		stopMinute = t.StopIssued.Truncate(time.Minute)
	}
	if r.Recorded, err = readRecorded(ctx, db, ids, split, stopMinute); err != nil {
		return err
	}
	if r.Windows, err = readWindows(ctx, rdb, ids); err != nil {
		return err
	}

	var requests []request
	for c := range 2 {
		for _, res := range results[c] {
			requests = append(requests, request{Copy: c, Result: res})
		}
	}
	e := evidence{Outage: s.Outage, Customers: demoCustomers, Requests: requests, Timeline: *t,
		Handlers: r.Handlers, Recorded: r.Recorded, Windows: r.Windows}
	restarts := "start time and restart count of " + strings.Join(apps, ", ") + " unchanged"
	if startsAfter != startsBefore {
		restarts = "before: " + strings.ReplaceAll(startsBefore, "\n", "; ") + "; after: " + strings.ReplaceAll(startsAfter, "\n", "; ")
	}
	r.Checks = append(reconcile(e),
		check{"apps reconnected without a restart", startsAfter == startsBefore, restarts},
		check{"no secret in the app logs", leaked == 0, fmt.Sprintf("admin token, database password and both API keys: %d found", leaked)})
	r.Tallies, r.Copies, r.Buckets = e.tallies(), copyRows(requests), buckets(e)
	r.FinishedAt = time.Now().UTC()
	return nil
}

// outage stops the dependency after s.Before, keeps it stopped for
// s.OutageFor, starts it again and waits until both copies are ready.
func outage(ctx context.Context, s settings, dc compose, w *watcher, client *http.Client, t *timeline, h *handlerCalls) error {
	name := service[s.Outage]
	if err := sleepUntil(ctx, t.LoadStart.Add(s.Before)); err != nil {
		return err
	}
	t.StopIssued = time.Now()
	fmt.Fprintf(os.Stderr, "%s stopping %s\n", since(t.LoadStart, t.StopIssued), name)
	if _, err := dc.run(ctx, "stop", name); err != nil {
		return err
	}
	t.Stopped = time.Now()
	var err error
	if t.Unready, err = w.waitBoth(ctx, http.StatusServiceUnavailable, 15*time.Second); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%s both copies not ready\n", since(t.LoadStart, t.Unready))
	t.QuietFrom = later(t.Unready, t.Stopped).Add(drain)
	t.QuietTo = t.LoadStart.Add(s.Before + s.OutageFor)
	if !t.QuietFrom.Before(t.QuietTo) {
		return errors.New("the outage ended before in-flight requests drained; use a longer -outage-for")
	}
	if err := sleepUntil(ctx, t.QuietFrom); err != nil {
		return err
	}
	if h.QuietFrom, err = readHandlerCalls(ctx, client); err != nil {
		return err
	}
	if err := sleepUntil(ctx, t.QuietTo); err != nil {
		return err
	}
	if h.QuietTo, err = readHandlerCalls(ctx, client); err != nil {
		return err
	}
	t.StartIssued = time.Now()
	fmt.Fprintf(os.Stderr, "%s starting %s\n", since(t.LoadStart, t.StartIssued), name)
	if _, err := dc.run(ctx, "up", "-d", "--no-deps", "--wait", name); err != nil {
		return err
	}
	t.Started = time.Now()
	if t.Ready, err = w.waitBoth(ctx, http.StatusOK, 30*time.Second); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%s both copies ready again\n", since(t.LoadStart, t.Ready))
	return nil
}

// cleanup removes the project's containers, network, volume and built images,
// then checks that Docker holds nothing for the project. It runs after
// Ctrl+C too, with its own deadline.
func cleanup(dc compose) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err := dc.run(ctx, "down", "-v", "--remove-orphans", "--rmi", "local"); err != nil {
		return "down failed", fmt.Errorf("cleanup: %w; remove the rest with:\n  %s", err, dc.downCommand())
	}
	found, err := leftovers(ctx)
	if err != nil {
		return "down done, leftover check failed", err
	}
	if len(found) > 0 {
		return "left behind: " + strings.Join(found, ", "), fmt.Errorf("cleanup left %s; remove it with:\n  %s", strings.Join(found, ", "), dc.downCommand())
	}
	return "removed; no container, volume, network or image of " + project + " left", nil
}

func databaseURL(password string) string {
	u := url.URL{Scheme: "postgres", User: url.UserPassword("quotagate", password),
		Host: "127.0.0.1:" + ports["QG_DB_PORT"], Path: "/quotagate", RawQuery: "sslmode=disable"}
	return u.String()
}

// preflight refuses a run that cannot be judged: a guarded example API that
// lets a request through without a key, or a run that would cross UTC
// midnight and reset the daily quota halfway.
func preflight(ctx context.Context, client *http.Client, db *pgxpool.Pool, total time.Duration) error {
	for _, origin := range exampleURLs {
		if status, err := call(ctx, client, "POST", origin+"/jobs", "", nil, nil); err != nil || status != http.StatusUnauthorized {
			return fmt.Errorf("preflight: %s/jobs without a key returned %d, want 401", origin, status)
		}
	}
	var untilMidnight float64
	if err := db.QueryRow(ctx, `SELECT extract(epoch FROM date_trunc('day', now() AT TIME ZONE 'UTC') + interval '1 day' - now() AT TIME ZONE 'UTC')::float8`).Scan(&untilMidnight); err != nil {
		return fmt.Errorf("read database clock: %w", err)
	}
	if time.Duration(untilMidnight*float64(time.Second)) < total+time.Minute {
		return errors.New("the run would cross UTC midnight; start after midnight")
	}
	return nil
}

func createCustomers(ctx context.Context, client *http.Client, adminToken string) (ids, keys []string, err error) {
	for _, c := range demoCustomers {
		var created struct {
			ID     string `json:"id"`
			APIKey string `json:"api_key"`
		}
		status, err := call(ctx, client, "POST", quotagateURLs[0]+"/v1/admin/customers", adminToken, map[string]string{"name": "qg13 " + c.Name}, &created)
		if err != nil || status != http.StatusCreated {
			return nil, nil, fmt.Errorf("create customer %s: status %d", c.Name, status)
		}
		policy := map[string]int64{"daily_limit": c.DailyLimit, "rate_limit_per_minute": c.RatePerMin}
		status, err = call(ctx, client, "PUT", quotagateURLs[0]+"/v1/admin/customers/"+created.ID+"/policies/job.create", adminToken, policy, nil)
		if err != nil || status != http.StatusOK {
			return nil, nil, fmt.Errorf("set policy for %s: status %d", c.Name, status)
		}
		ids, keys = append(ids, created.ID), append(keys, created.APIKey)
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

// readHandlerCalls sums the handler counts of both example APIs.
func readHandlerCalls(ctx context.Context, client *http.Client) (int64, error) {
	var sum int64
	for _, origin := range exampleURLs {
		var stats struct {
			HandlerCalls int64 `json:"handler_calls"`
		}
		if status, err := call(ctx, client, "GET", origin+"/demo/stats", "", nil, &stats); err != nil || status != http.StatusOK {
			return 0, fmt.Errorf("read handler count of %s: status %d", origin, status)
		}
		sum += stats.HandlerCalls
	}
	return sum, nil
}

func readRecorded(ctx context.Context, db *pgxpool.Pool, ids []string, split, stopMinute time.Time) ([]recorded, error) {
	out := make([]recorded, len(ids))
	for i, id := range ids {
		x := &out[i]
		err := db.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE allowed),
			count(*) FILTER (WHERE reason = 'daily_quota'), count(*) FILTER (WHERE reason = 'rate_limited'),
			count(*) FILTER (WHERE reason <> 'rate_limited' AND created_at <= $2),
			count(*) FILTER (WHERE reason <> 'rate_limited' AND created_at > $2),
			count(*) FILTER (WHERE reason = 'rate_limited' AND created_at <= $2),
			count(*) FILTER (WHERE reason = 'rate_limited' AND created_at > $2),
			count(*) FILTER (WHERE reason <> 'rate_limited' AND created_at >= $3 AND created_at <= $2),
			(SELECT coalesce(sum(used), 0)::bigint FROM quotagate.daily_usage WHERE customer_id = $1)
			FROM quotagate.decisions WHERE customer_id = $1`, id, split, stopMinute).
			Scan(&x.Decisions, &x.Allowed, &x.DailyQuota, &x.RateLimited,
				&x.PassedBefore, &x.PassedAfter, &x.LimitedBefore, &x.LimitedAfter, &x.PassedStopMinute, &x.DailyUsed)
		if err != nil {
			return nil, fmt.Errorf("read recorded decisions: %w", err)
		}
	}
	return out, nil
}

// readWindows counts each customer's Redis decision pre-results per
// one-minute window. Key names follow internal/ratelimit: hashes of the
// customer and decision IDs.
func readWindows(ctx context.Context, rdb *redis.Client, ids []string) ([][]window, error) {
	out := make([][]window, len(ids))
	for i, id := range ids {
		sum := sha256.Sum256([]byte(id))
		var keys []string
		iter := rdb.Scan(ctx, 0, "qg:rate:"+hex.EncodeToString(sum[:])+":decision:*", 1000).Iterator()
		for iter.Next(ctx) {
			keys = append(keys, iter.Val())
		}
		if err := iter.Err(); err != nil {
			return nil, fmt.Errorf("scan Redis pre-results: %w", err)
		}
		byStart := map[int64]*window{}
		for len(keys) > 0 {
			batch := keys[:min(500, len(keys))]
			keys = keys[len(batch):]
			values, err := rdb.MGet(ctx, batch...).Result()
			if err != nil {
				return nil, fmt.Errorf("read Redis pre-results: %w", err)
			}
			for _, v := range values {
				text, ok := v.(string)
				if !ok {
					continue // expired between SCAN and MGET
				}
				// Lua's cjson writes every number as a JSON number; read them as floats.
				var pre struct {
					Allowed float64 `json:"allowed"`
					StartMS float64 `json:"start_ms"`
				}
				if err := json.Unmarshal([]byte(text), &pre); err != nil {
					return nil, fmt.Errorf("decode Redis pre-result: %w", err)
				}
				start := int64(pre.StartMS)
				if byStart[start] == nil {
					byStart[start] = &window{Start: time.UnixMilli(start).UTC()}
				}
				if pre.Allowed == 1 {
					byStart[start].Passed++
				} else {
					byStart[start].Limited++
				}
			}
		}
		for _, w := range byStart {
			out[i] = append(out[i], *w)
		}
		sort.Slice(out[i], func(a, b int) bool { return out[i][a].Start.Before(out[i][b].Start) })
	}
	return out, nil
}

func sleepUntil(ctx context.Context, t time.Time) error {
	timer := time.NewTimer(time.Until(t))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
