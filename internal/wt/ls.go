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
	// PaneID is the workspace's first pane, which is the one staffing starts an
	// agent in and therefore the one `dispatch --pane` wants.
	PaneID      string
	AgentStatus string
	// PR is the branch's pull request state, and is only ever filled when the
	// caller asked for it — see WithPRs.
	PR  string
	Dir string
}

// Rows joins herdr's worktree list against its workspace list. A worktree
// carries open_workspace_id; the agent status lives on the workspace, so
// neither call alone answers which worktrees have an agent.
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
			PaneID:      missing,
			AgentStatus: missing,
			PR:          missing,
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

// WithPanes fills the pane column, which is what a dispatch needs. A workspace
// whose panes cannot be read keeps its "-": a failed lookup is not the same
// fact as a workspace holding no panes.
func WithPanes(client *herdrapi.Client, rows []Row) {
	// Several worktrees can name one workspace, so ask once per workspace.
	first := map[string]string{}
	for i, row := range rows {
		if row.WorkspaceID == missing {
			continue
		}
		pane, asked := first[row.WorkspaceID]
		if !asked {
			pane = firstPane(client, row.WorkspaceID)
			first[row.WorkspaceID] = pane
		}
		if pane != "" {
			rows[i].PaneID = pane
		}
	}
}

// firstPane is the pane staffing would target, empty when herdr cannot say.
func firstPane(client *herdrapi.Client, workspaceID string) string {
	panes, err := client.PaneList(workspaceID)
	if err != nil || len(panes.Panes) == 0 {
		return ""
	}
	return panes.Panes[0].PaneID
}

// WithPRs fills the pull request column from the supplied lookup. The main
// checkout is skipped: trunk has no pull request, and every lookup is a network
// round trip. A lookup with no answer leaves the "-" alone, so "gh could not
// say" and "no pull request" read alike — which is why doctor reports gh.
func WithPRs(rows []Row, lookup func(branch string) string) {
	for i, row := range rows {
		if row.Main || row.Branch == missing {
			continue
		}
		if state := lookup(row.Branch); state != "" {
			rows[i].PR = state
		}
	}
}

// Render writes the rows as an aligned table.
//
// The pull request column is omitted rather than dashed when it was not asked
// for: a "-" is the same cell a branch with no pull request prints, so the
// listing would read as a repository with no open work.
//
// Every cell is escaped. git accepts bidi overrides in a ref name, so a cell
// can otherwise draw as a branch it is not, and this listing is what a human
// reads before pruning. Per cell rather than per line, because the separators
// are themselves control characters.
func Render(w io.Writer, rows []Row, withPR bool) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, row := range rows {
		marker := " "
		if row.Main {
			marker = "*"
		}
		cells := []string{marker, safetext.Escape(row.Branch),
			safetext.Escape(row.WorkspaceID), safetext.Escape(row.PaneID),
			safetext.Escape(row.AgentStatus)}
		if withPR {
			cells = append(cells, safetext.Escape(row.PR))
		}
		cells = append(cells, safetext.Escape(row.Dir))
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	return tw.Flush()
}

// Ls lists every worktree of the repository containing dir, with the workspace,
// pane and agent state herdr has for each.
//
// root, when non-empty, is a repository root herdr already resolved from the
// invocation context; it saves a git call and works even from a directory git
// would refuse.
//
// lookupPR is nil unless the caller asked for pull request state, which costs a
// round trip per branch and is therefore never the default.
func Ls(client *herdrapi.Client, root, dir string, lookupPR func(branch string) string, out io.Writer) error {
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
	// session with nothing open.
	workspaces, err := client.WorkspaceList()
	if err != nil {
		return fmt.Errorf("list workspaces: %w", err)
	}

	rows := Rows(worktrees, workspaces)
	WithPanes(client, rows)
	if lookupPR != nil {
		WithPRs(rows, lookupPR)
	}
	return Render(out, rows, lookupPR != nil)
}

func derefOr(s *string, fallback string) string {
	if s == nil || strings.TrimSpace(*s) == "" {
		return fallback
	}
	return *s
}
