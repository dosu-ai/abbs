package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestUpsertCreatesSecureConfig(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "config", "abbs")
	path := filepath.Join(dir, "workspaces.toml")
	p := Profile{URL: "https://board.example", Username: "agent", TokenFile: "/secret/board.token", ReadOnly: true}
	if err := Upsert(path, "board", p); err != nil {
		t.Fatal(err)
	}

	profiles, names, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "board" || profiles["board"] != p {
		t.Fatalf("profiles = %#v, names = %v", profiles, names)
	}
	assertPerm(t, dir, 0o700)
	assertPerm(t, path, 0o600)
}

func TestUpsertPreservesOtherEntriesByteForByte(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workspaces.toml")
	original := []byte("# leading comment\n\n[workspaces.first]\n# keep this comment\nurl = \"https://first.example\"\ntoken_env = \"FIRST_TOKEN\"\n\n[workspaces.second]\nurl = \"https://second.example\"\ntoken = \"old\"\n\n[other]\nvalue = 1\n")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Upsert(path, "second", Profile{URL: "https://second.example", TokenFile: "/new.token"}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	prefix := "# leading comment\n\n[workspaces.first]\n# keep this comment\nurl = \"https://first.example\"\ntoken_env = \"FIRST_TOKEN\"\n\n"
	suffix := "[other]\nvalue = 1\n"
	if !strings.HasPrefix(string(got), prefix) || !strings.HasSuffix(string(got), suffix) {
		t.Fatalf("unrelated bytes changed:\n%s", got)
	}
	if strings.Contains(string(got), "token = \"old\"") || !strings.Contains(string(got), "token_file = \"/new.token\"") {
		t.Fatalf("target block was not replaced:\n%s", got)
	}
	assertPerm(t, path, 0o600)
}

func TestUpsertReplacesEquivalentQuotedTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workspaces.toml")
	original := []byte("# leading comment\n[workspaces.\"board\"] # quoted key\nurl = \"https://old.example\"\ntoken = \"old\"\n\n[workspaces.other]\nurl = \"https://other.example\"\ntoken = \"other\"\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	want := Profile{URL: "https://new.example", TokenFile: "/new.token", ReadOnly: true}
	if err := Upsert(path, "board", want); err != nil {
		t.Fatal(err)
	}
	profiles, names, err := Load(path)
	if err != nil {
		t.Fatalf("updated quoted table no longer loads: %v", err)
	}
	if len(names) != 2 || profiles["board"] != want || profiles["other"].URL != "https://other.example" {
		t.Fatalf("profiles = %#v, names = %v", profiles, names)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "https://old.example") {
		t.Fatalf("quoted target table was not replaced:\n%s", got)
	}
}

func TestConcurrentUpsertsPreserveEveryProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workspaces.toml")
	const count = 32
	start := make(chan struct{})
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			name := fmt.Sprintf("board-%02d", i)
			errs <- Upsert(path, name, Profile{URL: "https://" + name + ".example", TokenEnv: "TOKEN"})
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	profiles, names, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != count || len(names) != count {
		t.Fatalf("concurrent upserts retained %d profiles, want %d", len(profiles), count)
	}
}

func TestUpsertAppendsWithoutChangingExistingBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workspaces.toml")
	original := []byte("# hand written\n[workspaces.first]\nurl = \"https://first.example\"\ntoken = \"secret\"")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Upsert(path, "second", Profile{URL: "https://second.example", TokenFile: "/second.token"}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got), string(original)) {
		t.Fatalf("existing bytes changed:\n%s", got)
	}
	if !strings.Contains(string(got), "\n\n[workspaces.second]\n") {
		t.Fatalf("new block not separated:\n%s", got)
	}
}

func TestUpsertRenameFailureLeavesOriginalIntact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workspaces.toml")
	original := []byte("# precious\n[workspaces.board]\nurl = \"https://old.example\"\ntoken = \"old\"\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("injected rename failure")
	err := upsert(path, "board", Profile{URL: "https://new.example", TokenFile: "/new.token"}, func(_, _ string) error {
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want injected failure", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != string(original) {
		t.Fatalf("original changed after failed atomic write:\n%s", got)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".workspaces.toml.*.tmp"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary files left behind: %v, %v", matches, err)
	}
}

func TestWriteTokenIsAtomicAndPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "abbs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "board.token")
	if err := WriteToken(path, "secret-value"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "secret-value\n" {
		t.Fatalf("token contents = %q", got)
	}
	assertPerm(t, dir, 0o700)
	assertPerm(t, path, 0o600)
}

func TestTokenPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err := TokenPath("oss-board")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".config", "abbs", "oss-board.token")
	if got != want {
		t.Fatalf("TokenPath = %q, want %q", got, want)
	}
	if _, err := TokenPath("../escape"); err == nil {
		t.Fatal("unsafe profile name must fail")
	}
}

func assertPerm(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s permissions = %#o, want %#o", path, got, want)
	}
}
