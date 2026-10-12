package k8s_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

const namespace = "quotagate"

// TestKubernetesSmoke checks a running deploy/k8s install in the current
// kubectl context: two ready QuotaGate copies, allow and deny through the
// example API, and new decisions while and after one copy is deleted.
// Run with QG_K8S_SMOKE=1 after kubectl apply -k deploy/k8s.
func TestKubernetesSmoke(t *testing.T) {
	if os.Getenv("QG_K8S_SMOKE") != "1" {
		t.Skip("set QG_K8S_SMOKE=1 to run against the current Kubernetes context")
	}
	kubectl(t, "rollout", "status", "deploy/example-api", "--timeout=180s")
	before := twoSteadyPods(t)
	t.Logf("ready QuotaGate pods: %v", before)
	served := map[string]int{}
	for _, pod := range before {
		served[pod] = decisionsServed(t, pod)
	}

	adminToken := secret(t, "QG_ADMIN_TOKEN")
	quotagate := portForward(t, "svc/quotagate", 8080)
	example := portForward(t, "svc/example-api", 8081)
	run := time.Now().Format("150405")
	limited := createCustomer(t, quotagate, adminToken, "qg12 smoke "+run+" limited", 2)
	open := createCustomer(t, quotagate, adminToken, "qg12 smoke "+run+" open", 1000)
	t.Cleanup(func() {
		kubectl(t, "exec", "statefulset/postgres", "--", "psql", "-U", "quotagate", "-d", "quotagate", "-qc",
			"DELETE FROM quotagate.customers WHERE name LIKE 'qg12 smoke "+run+" %'")
	})

	// Allow, allow, then deny: the daily limit is two.
	for i, want := range []string{"201", "201", "429 daily_quota"} {
		if got := postJob(t, example, limited); got != want {
			t.Fatalf("limited job %d = %q, want %q", i+1, got, want)
		}
	}

	// Delete the copy that served those decisions, and keep deciding for 10s:
	// its 5s preStop wait, its shutdown, and the example API reconnecting
	// through the Service to the other copy.
	victim := busiest(t, served)
	kubectl(t, "delete", "pod", victim, "--wait=false")
	var during []string
	for range 20 {
		during = append(during, postJob(t, example, open))
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("decisions while %s stopped: %v", victim, during)
	for _, got := range during {
		if got != "201" {
			t.Fatalf("a decision failed while one copy was stopping: %v", during)
		}
	}

	after := twoSteadyPods(t)
	if strings.Contains(strings.Join(after, " "), victim) {
		t.Fatalf("ready pods after restart = %v, want two without %s", after, victim)
	}
	t.Logf("ready QuotaGate pods after restart: %v", after)
	for i := range 5 {
		if got := postJob(t, example, open); got != "201" {
			t.Fatalf("decision %d after restart = %q, want 201", i+1, got)
		}
	}
	for _, pod := range after {
		t.Logf("%s served %d decisions", pod, decisionsServed(t, pod))
	}
}

// busiest returns the pod that served the most decisions since its count in
// before was taken. The example API reuses its connection, so this is the copy
// it is talking to.
func busiest(t *testing.T, before map[string]int) string {
	t.Helper()
	best, most := "", 0
	for pod, n := range before {
		if d := decisionsServed(t, pod) - n; d > most {
			best, most = pod, d
		}
	}
	if best == "" {
		t.Fatal("no QuotaGate pod logged the decisions")
	}
	return best
}

func decisionsServed(t *testing.T, pod string) int {
	t.Helper()
	return strings.Count(kubectl(t, "logs", pod, "-c", "quotagate"), `"route":"/v1/decisions"`)
}

func kubectl(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("kubectl", append([]string{"-n", namespace}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// twoSteadyPods waits until exactly two QuotaGate pods exist and both are
// ready: none starting, none still stopping from an earlier rollout.
func twoSteadyPods(t *testing.T) []string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		out := kubectl(t, "get", "pods", "-l", "app=quotagate", "-o",
			`jsonpath={range .items[*]}{.metadata.name} {.metadata.deletionTimestamp} {.status.containerStatuses[0].ready}{"\n"}{end}`)
		lines := strings.Split(strings.TrimSpace(out), "\n")
		var ready []string
		for _, line := range lines {
			// A stopping pod has a deletion timestamp between name and readiness.
			if fields := strings.Fields(line); len(fields) == 2 && fields[1] == "true" {
				ready = append(ready, fields[0])
			}
		}
		if len(lines) == 2 && len(ready) == 2 {
			return ready
		}
		if time.Now().After(deadline) {
			t.Fatalf("QuotaGate pods never settled at two ready:\n%s", out)
		}
		time.Sleep(time.Second)
	}
}

// secret reads one value from deploy/k8s/secrets.env, the same file the
// Secret was generated from.
func secret(t *testing.T, key string) string {
	t.Helper()
	data, err := os.ReadFile("secrets.env")
	if err != nil {
		t.Fatalf("read secrets.env: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok && k == key {
			return v
		}
	}
	t.Fatalf("secrets.env has no %s", key)
	return ""
}

var forwarding = regexp.MustCompile(`Forwarding from 127\.0\.0\.1:(\d+)`)

// portForward opens a random local port to target and returns its base URL.
func portForward(t *testing.T, target string, port int) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "kubectl", "-n", namespace, "port-forward", target, fmt.Sprintf(":%d", port))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
	})
	found := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if m := forwarding.FindStringSubmatch(scanner.Text()); m != nil {
				found <- m[1]
			}
		}
	}()
	select {
	case local := <-found:
		return "http://127.0.0.1:" + local
	case <-time.After(20 * time.Second):
		t.Fatalf("port-forward to %s did not start", target)
		return ""
	}
}

func createCustomer(t *testing.T, base, adminToken, name string, dailyLimit int) string {
	t.Helper()
	var created struct {
		ID     string `json:"id"`
		APIKey string `json:"api_key"`
	}
	if status := call(t, "POST", base+"/v1/admin/customers", adminToken, map[string]any{"name": name}, &created); status != http.StatusCreated {
		t.Fatalf("create customer: status %d", status)
	}
	policy := map[string]any{"daily_limit": dailyLimit, "rate_limit_per_minute": 1000}
	if status := call(t, "PUT", base+"/v1/admin/customers/"+created.ID+"/policies/job.create", adminToken, policy, nil); status != http.StatusOK {
		t.Fatalf("set policy: status %d", status)
	}
	return created.APIKey
}

// postJob calls the example API and returns "201" or "429 <reason>" or the status.
func postJob(t *testing.T, base, key string) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	status := call(t, "POST", base+"/jobs", key, nil, &body)
	if status == http.StatusTooManyRequests {
		return "429 " + body.Error
	}
	return fmt.Sprint(status)
}

func call(t *testing.T, method, url, token string, in, out any) int {
	t.Helper()
	var reader io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(out)
	}
	return resp.StatusCode
}
