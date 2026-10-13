package envfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env.compose")
	content := "# local only\nQG_DB_PASSWORD=p@ss=w/rd\n  QG_ADMIN_TOKEN = \"admin-token\" \n\nnot a pair\nQG_DB_PORT='55433'\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	values, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"QG_DB_PASSWORD": "p@ss=w/rd", "QG_ADMIN_TOKEN": "admin-token", "QG_DB_PORT": "55433"}
	if len(values) != len(want) {
		t.Fatalf("values = %v", values)
	}
	for k, v := range want {
		if values[k] != v {
			t.Fatalf("%s = %q, want %q", k, values[k], v)
		}
	}
}

func TestReadMissingFile(t *testing.T) {
	if _, err := Read(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing file was accepted")
	}
}
