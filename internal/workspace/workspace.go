// Package workspace loads named workspace profiles for the multi-homed MCP
// adapter (M7). A workspace is a server; the adapter is an IRC client on
// several networks. Each profile is fully independent — URL, credentials,
// trust posture — and identity is per-workspace by construction.
package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Profile is one [workspaces.<name>] entry.
type Profile struct {
	URL string `toml:"url"`
	// Exactly one credential source: an inline token, a file holding it, or
	// the name of an environment variable holding it.
	Token     string `toml:"token"`
	TokenFile string `toml:"token_file"`
	TokenEnv  string `toml:"token_env"`
	// ReadOnly is the per-workspace trust posture: the adapter refuses every
	// write tool against this workspace (cheap containment for low-trust
	// workspaces; see DESIGN.md trust notes).
	ReadOnly bool `toml:"read_only"`
}

type file struct {
	Workspaces map[string]Profile `toml:"workspaces"`
}

// DefaultConfigPath is ~/.config/abbs/workspaces.toml (overridable via
// ABBS_CONFIG or the -config flag).
func DefaultConfigPath() string {
	if v := os.Getenv("ABBS_CONFIG"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "workspaces.toml"
	}
	return filepath.Join(home, ".config", "abbs", "workspaces.toml")
}

// Load parses the profiles file. Names are returned sorted for stable
// listings.
func Load(path string) (map[string]Profile, []string, error) {
	var f file
	if _, err := toml.DecodeFile(path, &f); err != nil {
		return nil, nil, fmt.Errorf("workspace config %s: %w", path, err)
	}
	if len(f.Workspaces) == 0 {
		return nil, nil, fmt.Errorf("workspace config %s: no [workspaces.<name>] entries", path)
	}
	names := make([]string, 0, len(f.Workspaces))
	for name, p := range f.Workspaces {
		if p.URL == "" {
			return nil, nil, fmt.Errorf("workspace %q: url is required", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return f.Workspaces, names, nil
}

// ResolveToken returns the profile's bearer token from whichever source is
// configured.
func (p Profile) ResolveToken() (string, error) {
	switch {
	case p.Token != "":
		return p.Token, nil
	case p.TokenFile != "":
		b, err := os.ReadFile(p.TokenFile)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	case p.TokenEnv != "":
		if v := os.Getenv(p.TokenEnv); v != "" {
			return v, nil
		}
		return "", fmt.Errorf("token_env %s is empty", p.TokenEnv)
	}
	return "", fmt.Errorf("no credential: set token, token_file, or token_env")
}

// CachePath names the per-workspace cache file. The hash covers URL and
// token, so a credential change (a different principal) discards the cache
// rather than serving another principal's visible slice — cursors from
// different servers must never mix.
func CachePath(name, url, token string) (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "abbs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(url + "\x00" + token))
	return filepath.Join(dir, fmt.Sprintf("%s-%s.db", name, hex.EncodeToString(sum[:6]))), nil
}
