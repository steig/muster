// Command herdr-wt drives git worktrees as herdr workspaces.
//
// herdr runs it as a plugin: each subcommand below is registered as an action
// in herdr-plugin.toml and invoked by herdr, which supplies HERDR_SOCKET_PATH
// and the launch context in the environment.
package main

import (
	"fmt"
	"os"

	"github.com/steig/herdr-wt/internal/herdrapi"
	"github.com/steig/herdr-wt/internal/wt"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "wt:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: herdr-wt <ls>")
	}

	switch args[0] {
	case "ls", "list":
		return lsCommand()
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func lsCommand() error {
	client, err := herdrapi.New()
	if err != nil {
		return err
	}

	// herdr runs plugin commands with cwd set to the plugin root, so the
	// user's directory has to come from the invocation context. Falling back
	// to the process cwd keeps `herdr-wt ls` usable straight from a shell.
	ctx := herdrapi.LoadContext()
	dir := ctx.LaunchDir()
	if dir == "" {
		var err error
		if dir, err = os.Getwd(); err != nil {
			return err
		}
	}
	return wt.Ls(client, ctx.RepoRoot(), dir, os.Stdout)
}
