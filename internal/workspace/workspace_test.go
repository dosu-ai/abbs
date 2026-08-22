package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAndResolve(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "oss.token")
	if err := os.WriteFile(tokenFile, []byte("abbs_filetoken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "workspaces.toml")
	conf := `
[workspaces.company]
url = "https://abbs.example.com"
token = "abbs_inline"

[workspaces.oss-foo]
url = "https://abbs.foo-project.org"
token_file = "` + tokenFile + `"
read_only = true
`
	if err := os.WriteFile(path, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}

	profiles, names, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "company" || names[1] != "oss-foo" {
		t.Fatalf("names = %v", names)
	}
	if tok, err := profiles["company"].ResolveToken(); err != nil || tok != "abbs_inline" {
		t.Fatalf("inline token = %q, %v", tok, err)
	}
	if tok, err := profiles["oss-foo"].ResolveToken(); err != nil || tok != "abbs_filetoken" {
		t.Fatalf("file token = %q, %v", tok, err)
	}
	if !profiles["oss-foo"].ReadOnly {
		t.Fatal("read_only must parse")
	}
	if _, err := (Profile{URL: "x"}).ResolveToken(); err == nil {
		t.Fatal("no credential must error")
	}
}

func TestLoadRejectsMissingURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w.toml")
	os.WriteFile(path, []byte("[workspaces.a]\ntoken = \"x\"\n"), 0o600)
	if _, _, err := Load(path); err == nil {
		t.Fatal("url-less profile must be rejected")
	}
}

func TestCachePathVariesByCredential(t *testing.T) {
	a, err := CachePath("w", "http://x", "token1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := CachePath("w", "http://x", "token2")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("a credential change must select a different cache file")
	}
}
