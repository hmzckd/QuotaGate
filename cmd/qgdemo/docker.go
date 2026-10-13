package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// project is the demo's own Compose project. Its containers, network and
// PostgreSQL volume are separate from every other QuotaGate project.
const project = "quotagate-qg13"

// ports keep the demo off the default 18080-18083 and 55432/56379 ports, so
// it can run while another QuotaGate project is up.
var ports = map[string]string{
	"QG_DB_PORT":        "25432",
	"QG_REDIS_PORT":     "26379",
	"QG_HTTP_PORT":      "28080",
	"QG_HTTP_2_PORT":    "28082",
	"QG_EXAMPLE_PORT":   "28081",
	"QG_EXAMPLE_2_PORT": "28084",
}

// apps are the services that must keep running, without a restart, through
// the dependency outage.
var apps = []string{"api", "api-2", "example-api", "example-api-2"}

type compose struct {
	envFile string
	files   []string
}

func newCompose(envFile, override string) compose {
	files := []string{"compose.yaml", "compose.qg08.yaml", "compose.qg13.yaml"}
	if override != "" {
		files = append(files, override)
	}
	return compose{envFile: envFile, files: files}
}

func (c compose) downCommand() string {
	return fmt.Sprintf("docker compose -p %s --env-file %s -f %s down -v --remove-orphans --rmi local",
		project, c.envFile, strings.Join(c.files, " -f "))
}

// run runs docker compose for the demo project and returns its stdout.
// Shell variables win over --env-file, so the demo ports apply even when
// .env.compose sets other ones.
func (c compose) run(ctx context.Context, args ...string) (string, error) {
	full := []string{"compose", "-p", project, "--env-file", c.envFile}
	for _, f := range c.files {
		full = append(full, "-f", f)
	}
	cmd := exec.CommandContext(ctx, "docker", append(full, args...)...)
	cmd.Env = os.Environ()
	for k, v := range ports {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker compose %s: %w\n%s", strings.Join(args, " "), err, tail(stderr.String(), 20))
	}
	return stdout.String(), nil
}

// leftovers lists what Docker still holds for the demo project. Images built
// by Compose carry the project label too.
func leftovers(ctx context.Context) ([]string, error) {
	var found []string
	for _, kind := range []string{"container", "volume", "network", "image"} {
		args := []string{kind, "ls", "-q", "--filter", "label=com.docker.compose.project=" + project}
		if kind == "container" {
			args = append(args, "-a")
		}
		out, err := exec.CommandContext(ctx, "docker", args...).Output()
		if err != nil {
			return nil, fmt.Errorf("docker %s ls: %w", kind, err)
		}
		if n := len(strings.Fields(string(out))); n > 0 {
			found = append(found, fmt.Sprintf("%d %s(s)", n, kind))
		}
	}
	return found, nil
}

// starts returns each app container's start time and restart count. If it
// is the same after the outage, the apps reconnected instead of restarting.
func (c compose) starts(ctx context.Context) (string, error) {
	ids, err := c.run(ctx, append([]string{"ps", "-q"}, apps...)...)
	if err != nil {
		return "", err
	}
	if n := len(strings.Fields(ids)); n != len(apps) {
		return "", fmt.Errorf("expected %d running app containers, found %d", len(apps), n)
	}
	args := append([]string{"inspect", "--format", "{{.Name}} {{.State.StartedAt}} restarts={{.RestartCount}}"}, strings.Fields(ids)...)
	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		return "", fmt.Errorf("docker inspect: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// secretsInLogs counts the given secrets found in the app containers' logs.
func (c compose) secretsInLogs(ctx context.Context, secrets ...string) (int, error) {
	logs, err := c.run(ctx, append([]string{"logs", "--no-color"}, apps...)...)
	if err != nil {
		return 0, err
	}
	found := 0
	for _, s := range secrets {
		if s == "" {
			return 0, errors.New("empty secret")
		}
		if strings.Contains(logs, s) {
			found++
		}
	}
	return found, nil
}

func tail(s string, lines int) string {
	parts := strings.Split(strings.TrimSpace(s), "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n")
}
