package wt

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/steig/worktender/internal/gitx"
	"github.com/steig/worktender/internal/herdrapi"
	"github.com/steig/worktender/internal/jsonout"
	"github.com/steig/worktender/internal/safetext"
)

// missing is printed for a column with nothing to show.
//
// It lives in the renderer and nowhere else. Inside a Row an absent fact is the
// empty string, because one "-" stands for four different absences — no
// workspace, no pane, no agent, no pull request — and a consumer needs them
// apart. A Row that stored the dash could only hand the ambiguity on.
const missing = "-"

// Row is one worktree joined with the herdr workspace that has it open. Every
// string field is empty when the fact is absent.
type Row struct {
	// Main is the repository's primary checkout rather than a linked worktree.
	Main        bool
	Branch      string
	WorkspaceID string
	// PaneID is the workspace's first pane, which is the one staffing starts an
	// agent in and therefore the one `dispatch --pane` wants.
	PaneID      string
	AgentStatus string
	// PR is nil unless a pull request lookup ran for this row — see WithPRs.
	PR  *PR
	Dir string
}

// PR is what a pull request lookup established about a branch.
//
// A nil *PR is not an empty one: nobody asked, versus asked and told there is
// none. Err set means gh could not be asked at all — the fact the table's "-"
// cannot carry, since an unauthenticated gh reads there as "no pull request",
// and that is what makes prune keep everything.
type PR struct {
	// State is gh's own spelling — OPEN, MERGED, CLOSED — and empty when the
	// branch has no pull request. Meaningless when Err is set.
	State string
	Err   error
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
			Branch:      deref(w.Branch),
			WorkspaceID: deref(w.OpenWorkspaceID),
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
// whose panes cannot be read keeps its empty pane: a failed lookup is not the
// same fact as a workspace holding no panes.
func WithPanes(client *herdrapi.Client, rows []Row) {
	// Several worktrees can name one workspace, so ask once per workspace.
	first := map[string]string{}
	for i, row := range rows {
		if row.WorkspaceID == "" {
			continue
		}
		pane, asked := first[row.WorkspaceID]
		if !asked {
			pane = firstPane(client, row.WorkspaceID)
			first[row.WorkspaceID] = pane
		}
		rows[i].PaneID = pane
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
// round trip — which is why its PR stays nil rather than becoming an answer.
//
// A lookup that failed is recorded as a failure rather than discarded. The
// table still prints "-" for it, having one column and no room to say more, but
// the JSON says which of the two it was.
func WithPRs(rows []Row, lookup func(branch string) (string, error)) {
	for i, row := range rows {
		if row.Main || row.Branch == "" {
			continue
		}
		state, err := lookup(row.Branch)
		rows[i].PR = &PR{State: state, Err: err}
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
		cells := []string{marker, cell(row.Branch), cell(row.WorkspaceID),
			cell(row.PaneID), cell(row.AgentStatus)}
		if withPR {
			cells = append(cells, cell(prCell(row.PR)))
		}
		cells = append(cells, cell(row.Dir))
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	return tw.Flush()
}

// cell is one column's text: escaped, and a dash when there is nothing to show.
func cell(s string) string {
	if s == "" {
		return missing
	}
	return safetext.Escape(s)
}

// prCell is the pull request column, empty for every case the table has no room
// to distinguish — not asked, asked and told none, and gh unable to answer.
func prCell(pr *PR) string {
	if pr == nil || pr.Err != nil {
		return ""
	}
	return pr.State
}

// ListJSON is what `ls --json` writes. An object rather than a bare array, so a
// later field has somewhere to go that does not change the type of the document.
type ListJSON struct {
	Worktrees []RowJSON `json:"worktrees"`
}

// RowJSON is Row as a consumer reads it. Absence is null rather than "-",
// because the dash means four different things and JSON has a word for none of
// them.
//
// Values are raw, not terminal-escaped: this is data. A branch name with the
// escaping applied is no longer a branch name that can be handed back to git,
// and rendering a string a stranger chose is the consumer's job — the same job
// Render does for the table.
//
// The shape may move before 1.0.
type RowJSON struct {
	Main        bool    `json:"main"`
	Branch      *string `json:"branch"`
	WorkspaceID *string `json:"workspace_id"`
	PaneID      *string `json:"pane_id"`
	AgentStatus *string `json:"agent_status"`
	// PR is null when no lookup ran for this row: --pr was not passed, or this
	// is the main checkout, which is never asked about.
	PR  *PRJSON `json:"pr"`
	Dir string  `json:"dir"`
}

// PRJSON is a pull request lookup's answer.
type PRJSON struct {
	// State is null when the branch has no pull request, and meaningless when
	// Error is set.
	State *string `json:"state"`
	// Error is why gh could not be asked. This is the distinction the table
	// loses: a gh that is missing or unauthenticated reads there as "no pull
	// request", and that is the reading that makes prune keep everything.
	Error *string `json:"error"`
}

// JSON projects rows for a machine.
//
// It is a view of exactly the []Row the table renders, built after the same
// lookups, so the two cannot disagree about what herdr said.
func JSON(rows []Row) []RowJSON {
	out := make([]RowJSON, 0, len(rows))
	for _, row := range rows {
		out = append(out, RowJSON{
			Main:        row.Main,
			Branch:      jsonout.String(row.Branch),
			WorkspaceID: jsonout.String(row.WorkspaceID),
			PaneID:      jsonout.String(row.PaneID),
			AgentStatus: jsonout.String(row.AgentStatus),
			PR:          prJSON(row.PR),
			Dir:         row.Dir,
		})
	}
	return out
}

func prJSON(pr *PR) *PRJSON {
	if pr == nil {
		return nil
	}
	if pr.Err != nil {
		return &PRJSON{Error: jsonout.String(pr.Err.Error())}
	}
	return &PRJSON{State: jsonout.String(pr.State)}
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
func Ls(client *herdrapi.Client, root, dir string, lookupPR func(branch string) (string, error), asJSON bool, out io.Writer) error {
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

	// Not degraded to an empty status column: that is the same row a worktree
	// with no workspace prints, so a herdr that failed to answer would read as
	// a session with nothing open.
	workspaces, err := client.WorkspaceList()
	if err != nil {
		return fmt.Errorf("list workspaces: %w", err)
	}

	rows := Rows(worktrees, workspaces)
	WithPanes(client, rows)
	if lookupPR != nil {
		WithPRs(rows, lookupPR)
	}
	if asJSON {
		return jsonout.Write(out, ListJSON{Worktrees: JSON(rows)})
	}
	return Render(out, rows, lookupPR != nil)
}

func deref(s *string) string {
	if s == nil || strings.TrimSpace(*s) == "" {
		return ""
	}
	return *s
}
