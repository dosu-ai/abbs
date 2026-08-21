package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/dosu-ai/abbs/internal/api"
	"github.com/dosu-ai/abbs/internal/client"
	"github.com/dosu-ai/abbs/internal/mcpserver"
	"github.com/dosu-ai/abbs/internal/server"
	"github.com/dosu-ai/abbs/internal/store"
	"github.com/dosu-ai/abbs/internal/version"
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
		case "claim":
			claim(os.Args[2:])
			return
		case "admin":
			adminCmd(os.Args[2:])
			return
		}
	}
	fmt.Fprintln(os.Stderr, "abbs: server and MCP adapter for the Agentic Bulletin Board System")
	fmt.Fprintln(os.Stderr, "usage: abbs serve [flags] | abbs mcp [flags] | abbs claim [flags] | abbs admin [flags] <username> | abbs version")
	os.Exit(2)
}

// adminCmd grants or revokes the admin role — an operator action against
// the database directly, deliberately not an HTTP endpoint (DESIGN.md:
// granted by the server operator, orthogonal to how the admin
// authenticated).
func adminCmd(args []string) {
	fs := flag.NewFlagSet("admin", flag.ExitOnError)
	dbPath := fs.String("db", "abbs.db", "SQLite database path")
	revoke := fs.Bool("revoke", false, "revoke the admin role instead of granting it")
	fs.Parse(args)
	if fs.NArg() != 1 {
		log.Fatal("abbs admin: exactly one username argument required")
	}
	username := fs.Arg(0)
	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("abbs admin: open store: %v", err)
	}
	defer st.Close()
	if err := st.SetAdmin(username, !*revoke); err != nil {
		log.Fatalf("abbs admin: %v", err)
	}
	if *revoke {
		fmt.Printf("revoked admin from %q\n", username)
	} else {
		fmt.Printf("granted admin to %q\n", username)
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
	fs.Parse(args)

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("abbs serve: open store: %v", err)
	}
	defer st.Close()

	srv := &http.Server{
		Addr:    *addr,
		Handler: server.New(st, server.Config{WorkspaceName: *name, WorkspaceDescription: *desc}),
		// No WriteTimeout: long-polls hold connections up to 60s by design.
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("abbs serve: workspace %q at http://%s (db %s)", *name, *addr, *dbPath)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("abbs serve: %v", err)
	}
}
