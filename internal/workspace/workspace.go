// Package workspace loads named workspace profiles for the multi-homed MCP
// adapter (M7). A workspace is a server; the adapter is an IRC client on
// several networks. Each profile is fully independent — URL, credentials,
// trust posture — and identity is per-workspace by construction.
package workspace

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Profile is one [workspaces.<name>] entry.
type Profile struct {
	URL string `toml:"url"`
	// Username is local metadata written by `abbs connect`. Older, hand-written
	// profiles may omit it; the MCP adapter does not depend on it.
	Username string `toml:"username"`
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

var profileNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

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

// TokenPath returns the default credential path for a connected profile.
// Tokens deliberately live outside the profiles file so MCP configuration
// and hand-written TOML never need to contain the secret itself.
func TokenPath(name string) (string, error) {
	if !profileNameRE.MatchString(name) {
		return "", fmt.Errorf("invalid workspace profile %q: use lowercase letters, numbers, and hyphens", name)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".config", "abbs", name+".token"), nil
}

// Upsert atomically adds or replaces one [workspaces.<name>] block while
// preserving every byte outside that block. This is intentionally not a
// parse-and-re-emit operation: profiles files are user-authored and commonly
// carry comments and formatting that a TOML encoder would discard.
func Upsert(path, name string, p Profile) error {
	return upsert(path, name, p, os.Rename)
}

func upsert(path, name string, p Profile, rename func(string, string) error) error {
	if !profileNameRE.MatchString(name) {
		return fmt.Errorf("invalid workspace profile %q: use lowercase letters, numbers, and hyphens", name)
	}
	if p.URL == "" {
		return fmt.Errorf("workspace %q: url is required", name)
	}
	unlock, err := lockConfig(path)
	if err != nil {
		return fmt.Errorf("lock workspace config %s: %w", path, err)
	}
	defer unlock()

	original, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read workspace config %s: %w", path, err)
	}
	updated := replaceBlock(original, name, profileBlock(name, p))
	if err := atomicWrite(path, updated, 0o600, false, rename); err != nil {
		return fmt.Errorf("write workspace config %s: %w", path, err)
	}
	return nil
}

// WriteToken atomically writes a bearer token with owner-only permissions.
func WriteToken(path, token string) error {
	if token == "" {
		return fmt.Errorf("refusing to write an empty token")
	}
	if err := atomicWrite(path, []byte(token+"\n"), 0o600, true, os.Rename); err != nil {
		return fmt.Errorf("write token %s: %w", path, err)
	}
	return nil
}

func atomicWrite(path string, contents []byte, mode os.FileMode, privateDir bool, rename func(string, string) error) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if privateDir {
		if err := os.Chmod(dir, 0o700); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpPath)
	}()
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if _, err := tmp.Write(contents); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return rename(tmpPath, path)
}

func profileBlock(name string, p Profile) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "[workspaces.%s]\n", name)
	fmt.Fprintf(&b, "url = %s\n", strconv.Quote(p.URL))
	if p.Username != "" {
		fmt.Fprintf(&b, "username = %s\n", strconv.Quote(p.Username))
	}
	if p.Token != "" {
		fmt.Fprintf(&b, "token = %s\n", strconv.Quote(p.Token))
	}
	if p.TokenFile != "" {
		fmt.Fprintf(&b, "token_file = %s\n", strconv.Quote(p.TokenFile))
	}
	if p.TokenEnv != "" {
		fmt.Fprintf(&b, "token_env = %s\n", strconv.Quote(p.TokenEnv))
	}
	if p.ReadOnly {
		b.WriteString("read_only = true\n")
	}
	return []byte(b.String())
}

func replaceBlock(original []byte, name string, block []byte) []byte {
	start, end := -1, -1
	for offset := 0; offset < len(original); {
		next := bytes.IndexByte(original[offset:], '\n')
		lineEnd := len(original)
		if next >= 0 {
			lineEnd = offset + next + 1
		}
		key, tableType, isTable := parseTableHeader(original[offset:lineEnd])
		isTarget := isTable && tableType == "Hash" && len(key) == 2 && key[0] == "workspaces" && key[1] == name
		if start < 0 && isTarget {
			start = offset
		} else if start >= 0 && isTable {
			end = offset
			break
		}
		offset = lineEnd
	}
	if start >= 0 {
		if end < 0 {
			end = len(original)
		}
		out := make([]byte, 0, start+len(block)+len(original)-end)
		out = append(out, original[:start]...)
		out = append(out, block...)
		out = append(out, original[end:]...)
		return out
	}

	out := append([]byte(nil), original...)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	if len(out) > 0 && (len(out) < 2 || out[len(out)-2] != '\n') {
		out = append(out, '\n')
	}
	return append(out, block...)
}

// parseTableHeader lets the TOML parser decide whether a line is a table
// header and returns its semantic key. In particular, workspaces.board and
// workspaces."board" are the same TOML key even though their bytes differ.
func parseTableHeader(line []byte) (toml.Key, string, bool) {
	if !strings.HasPrefix(strings.TrimSpace(string(line)), "[") {
		return nil, "", false
	}
	var parsed map[string]any
	metadata, err := toml.Decode(string(line), &parsed)
	if err != nil {
		return nil, "", false
	}
	keys := metadata.Keys()
	if len(keys) != 1 {
		return nil, "", false
	}
	tableType := metadata.Type(keys[0]...)
	if tableType != "Hash" && tableType != "ArrayHash" {
		return nil, "", false
	}
	return keys[0], tableType, true
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
