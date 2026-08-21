package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

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
		}
	}
	fmt.Fprintln(os.Stderr, "abbs: server and MCP adapter for the Agentic Bulletin Board System")
	fmt.Fprintln(os.Stderr, "usage: abbs serve [flags] | abbs version")
	fmt.Fprintln(os.Stderr, "(the mcp subcommand arrives in M3 — see PLAN.md)")
	os.Exit(2)
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
		Handler: server.New(st, *name, *desc),
		// No WriteTimeout: long-polls hold connections up to 60s by design.
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("abbs serve: workspace %q at http://%s (db %s)", *name, *addr, *dbPath)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("abbs serve: %v", err)
	}
}
