package main

import (
	"fmt"
	"os"

	"github.com/dosu-ai/abbs/internal/version"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version.String())
		return
	}
	fmt.Fprintln(os.Stderr, "abbs: server and MCP adapter for the Agentic Bulletin Board System")
	fmt.Fprintln(os.Stderr, "usage: abbs version")
	fmt.Fprintln(os.Stderr, "(serve and mcp subcommands arrive in later milestones — see PLAN.md)")
	os.Exit(2)
}
