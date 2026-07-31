package wt

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/steig/worktender/internal/gitx"
	"github.com/steig/worktender/internal/herdrapi"
	"github.com/steig/worktender/internal/safetext"
)

// missing is printed for a column with nothing to show.
const missing = "-"

// Row is one worktree joined with the herdr workspace that has it open.
type Row struct {
	// Main is the repository's primary checkout rather than a linked worktree.
	Main        bool
	Branch      string
	WorkspaceID string
	AgentStatus string
	Dir         string
}

// Rows joins herdr's worktree list against its workspace list.
//
// A worktree carries open_workspace_id; the agent status lives on the
// workspace. Neither call alone can answer "which worktrees have an agent and
// what is it doing", which is the whole point of the listing.
func Rows(worktrees *herdrapi.WorktreeListResponse, workspaces *herdrapi.WorkspaceListResponse) []Row {
	byID := map[string]herdrapi.WorkspaceInfo{}
	if workspaces != nil {
		for _, ws := range workspaces.Workspaces {
			byID[ws.WorkspaceID] = ws
		}
	}

	rows := make([]Row, 0, len(worktrees.Worktrees))
	for _, w := range worktrees.Worktrees {
		row := Row{
			Main:        !w.IsLinkedWorktree,
			Branch:      derefOr(w.Branch, missing),
			WorkspaceID: derefOr(w.OpenWorkspaceID, missing),
			AgentStatus: missing,
			Dir:         filepath.Base(w.Path),
		}
		// A worktree can name a workspace herdr has since closed, so only
		// trust a status we actually found.
		if w.OpenWorkspaceID != nil {
			if ws, ok := byID[*w.OpenWorkspaceID]; ok {
				row.AgentStatus = string(ws.AgentStatus)
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// Render writes the rows as an aligned table.
//
// Every cell is escaped on the way out. git accepts bidi overrides in a ref
// name and a directory can hold anything at all, so a cell can otherwise draw
// as a branch it is not — and this listing is what a human reads before
// pruning. The escape is applied per cell rather than per line because the
// separators the table is built from are themselves control characters.
func Render(w io.Writer, rows []Row) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, row := range rows {
		marker := " "
		if row.Main {
			marker = "*"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			marker, safetext.Escape(row.Branch), safetext.Escape(row.WorkspaceID),
			safetext.Escape(row.AgentStatus), safetext.Escape(row.Dir))
	}
	return tw.Flush()
}

// Ls lists every worktree of the repository containing dir, with the workspace
// and agent state herdr has for each.
//
// root, when non-empty, is a repository root herdr already resolved from the
// invocation context; it saves a git call and works even from a directory git
// would refuse.
func Ls(client *herdrapi.Client, root, dir string, out io.Writer) error {
	if root == "" {
		var err error
		if root, err = gitx.RepoRoot(dir); err != nil {
			return err
		}
	}

	worktrees, err := client.WorktreeList(root)
	if err != nil {
		return fmt.Errorf("list worktrees: %w", err)
	}

	// Not degraded to a "-" status column: that is the same row a worktree with
	// no workspace prints, so a herdr that failed to answer would read as a
	// session with nothing open — an invitation to sync worktrees that may
	// already have agents.
	workspaces, err := client.WorkspaceList()
	if err != nil {
		return fmt.Errorf("list workspaces: %w", err)
	}

	return Render(out, Rows(worktrees, workspaces))
}

func derefOr(s *string, fallback string) string {
	if s == nil || strings.TrimSpace(*s) == "" {
		return fallback
	}
	return *s
}
