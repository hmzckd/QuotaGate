package main

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func TestSecretsFromComposeEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env.compose")
	content := "# local only\nQG_DB_PASSWORD=p@ss:w/rd\nQG_ADMIN_TOKEN=\"admin-token\"\nQG_DB_PORT=55433\n\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	token, databaseURL, err := secrets(path)
	if err != nil {
		t.Fatal(err)
	}
	if token != "admin-token" {
		t.Fatalf("token = %q", token)
	}
	u, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	password, _ := u.User.Password()
	if u.User.Username() != "quotagate" || password != "p@ss:w/rd" || u.Host != "127.0.0.1:55433" || u.Path != "/quotagate" {
		t.Fatalf("database URL parts: user %q host %q path %q", u.User.Username(), u.Host, u.Path)
	}
}

func TestSecretsNeedBothValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env.compose")
	if err := os.WriteFile(path, []byte("QG_DB_PASSWORD=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := secrets(path); err == nil {
		t.Fatal("missing admin token was accepted")
	}
	t.Setenv("ADMIN_TOKEN", "")
	t.Setenv("DATABASE_URL", "")
	if _, _, err := secrets(""); err == nil {
		t.Fatal("empty environment was accepted")
	}
}
