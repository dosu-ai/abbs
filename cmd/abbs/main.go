package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/dosu-ai/abbs/internal/api"
	"github.com/dosu-ai/abbs/internal/client"
	"github.com/dosu-ai/abbs/internal/mcpserver"
	"github.com/dosu-ai/abbs/internal/server"
	"github.com/dosu-ai/abbs/internal/store"
	"github.com/dosu-ai/abbs/internal/ui"
	"github.com/dosu-ai/abbs/internal/version"
	"github.com/dosu-ai/abbs/internal/workspace"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version":
			fmt.Println(version.String())
			return
		case "serve":
			serve(os.Args[2:])
			return
		case "mcp":
			if err := mcpserver.Run(os.Args[2:]); err != nil {
				log.Fatalf("abbs mcp: %v", err)
			}
			return
		case "ui":
			uiCmd(os.Args[2:])
			return
		case "claim":
			claim(os.Args[2:])
			return
		case "admin":
			adminCmd(os.Args[2:])
			return
		}
	}
	fmt.Fprintln(os.Stderr, "abbs: server, MCP adapter, and development UI for the Agent Bulletin Board System")
	fmt.Fprintln(os.Stderr, "usage: abbs serve [flags] | abbs ui [flags] | abbs mcp [flags] | abbs claim [flags] | abbs admin <subcommand> | abbs version")
	os.Exit(2)
}

// uiCmd serves the local, read-only development viewer. Workspace profiles
// are re-read by the handler on every page request, so editing the TOML only
// requires a browser refresh.
func uiCmd(args []string) {
	fs := flag.NewFlagSet("ui", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8090", "listen address")
	configPath := fs.String("config", workspace.DefaultConfigPath(), "workspace profiles file (env ABBS_CONFIG)")
	fs.Parse(args)

	handler, err := ui.New(ui.Config{ConfigPath: *configPath})
	if err != nil {
		log.Fatalf("abbs ui: %v", err)
	}
	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("abbs ui: read-only viewer at http://%s (profiles %s)", *addr, *configPath)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("abbs ui: %v", err)
	}
}

const adminUsage = `usage: abbs admin <subcommand> [flags] <username>

Operator actions against the database directly — deliberately not HTTP
endpoints (DESIGN.md: the admin role is granted by the server operator,
orthogonal to how the admin authenticated).

  grant        grant the admin role
  revoke       revoke the admin role
  create-user  create a user and mint their API key (stdout is the key alone)
  rotate-key   mint a replacement API key, revoking the old immediately
`

func adminCmd(args []string) {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, adminUsage)
		os.Exit(2)
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("admin "+sub, flag.ExitOnError)
	dbPath := fs.String("db", "abbs.db", "SQLite database path")
	kind := fs.String("kind", "agent", `create-user: principal kind, "human" or "agent"`)
	displayName := fs.String("display-name", "", "create-user: optional display name")
	admin := fs.Bool("admin", false, "create-user: also grant the admin role")
	fs.Parse(args)
	if fs.NArg() != 1 {
		log.Fatalf("abbs admin %s: exactly one username argument required", sub)
	}
	username := fs.Arg(0)
	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("abbs admin: open store: %v", err)
	}
	defer st.Close()

	switch sub {
	case "grant", "revoke":
		if err := st.SetAdmin(username, sub == "grant"); err != nil {
			log.Fatalf("abbs admin %s: %v", sub, err)
		}
		if sub == "grant" {
			fmt.Printf("granted admin to %q\n", username)
		} else {
			fmt.Printf("revoked admin from %q\n", username)
		}
	case "create-user":
		token, tokenHash := server.NewToken()
		var dn *string
		if *displayName != "" {
			dn = displayName
		}
		if _, err := st.ClaimUser(username, *kind, dn, tokenHash, time.Now()); err != nil {
			log.Fatalf("abbs admin create-user: %v", err)
		}
		if *admin {
			if err := st.SetAdmin(username, true); err != nil {
				log.Fatalf("abbs admin create-user: grant admin: %v", err)
			}
		}
		fmt.Fprintf(os.Stderr, "created %q (%s) — the API key below is shown once; store it safely:\n", username, *kind)
		fmt.Println(token)
	case "rotate-key":
		token, tokenHash := server.NewToken()
		if err := st.RotateToken(username, tokenHash); err != nil {
			log.Fatalf("abbs admin rotate-key: %v", err)
		}
		fmt.Fprintf(os.Stderr, "rotated the API key for %q — the old key is revoked; the new one is shown once:\n", username)
		fmt.Println(token)
	default:
		fmt.Fprint(os.Stderr, adminUsage)
		os.Exit(2)
	}
}

// claim is a convenience for the first-claim ceremony: claim an identity
// and print the bearer token (stdout is the token alone, for scripting).
func claim(args []string) {
	fs := flag.NewFlagSet("claim", flag.ExitOnError)
	urlFlag := fs.String("url", "http://127.0.0.1:8080", "workspace server base URL")
	username := fs.String("username", "", "username to claim (required)")
	kind := fs.String("kind", "agent", `principal kind: "human" or "agent"`)
	displayName := fs.String("display-name", "", "optional display name")
	fs.Parse(args)
	if *username == "" {
		log.Fatal("abbs claim: -username is required")
	}
	req := api.ClaimUserRequest{Username: *username, Kind: *kind}
	if *displayName != "" {
		req.DisplayName = displayName
	}
	c := &client.Client{BaseURL: *urlFlag}
	resp, err := c.ClaimUser(context.Background(), req)
	if err != nil {
		log.Fatalf("abbs claim: %v", err)
	}
	fmt.Fprintf(os.Stderr, "claimed %q (%s) on %s — the token below is shown once; store it safely:\n",
		resp.User.Username, resp.User.Kind, *urlFlag)
	fmt.Println(resp.Token)
}

// serve runs the local server. Zero config = SQLite in ./abbs.db with
// first-claim auth — the "simple server" configuration.
func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "listen address")
	dbPath := fs.String("db", "abbs.db", "SQLite database path")
	name := fs.String("workspace", "abbs", "workspace name (a workspace is a server)")
	desc := fs.String("description", "", "workspace description")
	visibility := fs.String("visibility", server.VisibilityPrivate, `workspace visibility: "private" or "public"`)
	canonicalURL := fs.String("canonical-url", "", "optional HTTPS workspace origin (required for public visibility)")
	directoryListing := fs.Bool("directory-listing", false, "consent to third-party directory listing (public visibility and description required)")
	trustedProxyCIDRs := fs.String("trusted-proxy-cidrs", "", "comma-separated proxy CIDRs allowed to supply X-Forwarded-For")
	authMode := fs.String("auth", server.AuthFirstClaim,
		`auth mode: "first-claim" (anyone may claim an unclaimed name — localhost only) or "api-key" (admin-issued keys via abbs admin create-user)`)
	fs.Parse(args)
	if *name == "" {
		log.Fatal("abbs serve: -workspace must be 1..100 Unicode code points")
	}
	if *authMode != server.AuthFirstClaim && *authMode != server.AuthAPIKey {
		log.Fatalf("abbs serve: -auth must be %q or %q", server.AuthFirstClaim, server.AuthAPIKey)
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("abbs serve: open store: %v", err)
	}
	defer st.Close()
	var trustedProxies []string
	for _, cidr := range strings.Split(*trustedProxyCIDRs, ",") {
		if cidr = strings.TrimSpace(cidr); cidr != "" {
			trustedProxies = append(trustedProxies, cidr)
		}
	}
	handler, err := server.New(st, server.Config{
		WorkspaceName: *name, WorkspaceDescription: *desc,
		WorkspaceVisibility: *visibility, WorkspaceCanonicalURL: *canonicalURL,
		WorkspaceDirectoryListing: *directoryListing, AuthMode: *authMode,
		TrustedProxyCIDRs: trustedProxies,
	})
	if err != nil {
		log.Fatalf("abbs serve: configuration: %v", err)
	}

	srv := &http.Server{
		Addr:    *addr,
		Handler: handler,
		// No WriteTimeout: long-polls hold connections up to 60s by design.
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("abbs serve: workspace %q at http://%s (db %s, auth %s, visibility %s)", *name, *addr, *dbPath, *authMode, *visibility)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("abbs serve: %v", err)
	}
}
