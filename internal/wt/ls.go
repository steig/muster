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

// Options is what a listing was asked for beyond which repositories to read.
//
// A struct rather than two more parameters: both are booleans, and `nil, true,
// false, true` at a call site says nothing about which switch is which. The
// pull request lookup is deliberately not here — see LsAll.
type Options struct {
	// Blocked keeps only the worktrees herdr reports a blocked agent in.
	Blocked bool
	JSON    bool
}

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

// OnlyBlocked keeps the worktrees herdr reports a blocked agent in.
//
// This is herdr's own agent status for the workspace, not worktender's `report
// --status blocked` envelope. The two are different signals and nothing here
// folds one into the other: herdr's says a session has stopped and only a
// person can restart it, while the envelope is a worker telling whichever
// coordinator gated on it why it gave up — and reaches nobody else.
//
// It is the status worth a filter of its own because it is the one that stays
// put. Working resolves itself, idle is finished or waiting to be told what to
// do, and blocked sits there until somebody looks.
func OnlyBlocked(rows []Row) []Row {
	kept := make([]Row, 0, len(rows))
	for _, row := range rows {
		if row.AgentStatus == string(herdrapi.AgentStatusBlocked) {
			kept = append(kept, row)
		}
	}
	return kept
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
	renderRows(tw, rows, withPR, "")
	return tw.Flush()
}

// renderRows writes the row lines into an open tabwriter, each marker cell
// prefixed with indent. Shared so the grouped listing draws the same columns as
// the flat one rather than a second table that resembles it.
func renderRows(w io.Writer, rows []Row, withPR bool, indent string) {
	for _, row := range rows {
		marker := indent + " "
		if row.Main {
			marker = indent + "*"
		}
		cells := []string{marker, cell(row.Branch), cell(row.WorkspaceID),
			cell(row.PaneID), cell(row.AgentStatus)}
		if withPR {
			cells = append(cells, cell(prCell(row.PR)))
		}
		cells = append(cells, cell(row.Dir))
		fmt.Fprintln(w, strings.Join(cells, "\t"))
	}
}

// RenderRepos writes a cross-repository listing: a line naming the repository,
// then its rows indented beneath it. Grouped rather than given a repository
// column because a repository that could not be read has no rows to hang the
// name off, and that is the row the reader most needs.
//
// The full root rather than its basename: two checkouts on one machine
// routinely share a basename — `web`, `api` — and this listing exists to be
// read across all of them at once.
//
// A repository whose rows were all filtered away is skipped rather than drawn
// as a bare heading. `--blocked` across six repositories is a question with a
// short answer, and five empty headings bury it. A repository that could not be
// read is never skipped.
func RenderRepos(w io.Writer, repos []Repo, opts Options) error {
	if len(repos) == 0 {
		fmt.Fprintln(w, "no repositories: herdr has no worktree workspaces open")
		return nil
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	printed := 0
	for _, repo := range repos {
		if opts.Blocked && repo.Err == "" && len(repo.Rows) == 0 {
			continue
		}
		printed++
		fmt.Fprintln(tw, safetext.Escape(repo.Root))
		if repo.Err != "" {
			fmt.Fprintf(tw, "  cannot be read: %s\n", safetext.Escape(repo.Err))
			continue
		}
		renderRows(tw, repo.Rows, false, "  ")
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	// Silence is not an answer here: the same empty output is what a listing
	// that failed to reach anything would print.
	if printed == 0 {
		noun := "repositories"
		if len(repos) == 1 {
			noun = "repository"
		}
		fmt.Fprintf(w, "no blocked agents in %d %s\n", len(repos), noun)
	}
	return nil
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

// Repo is one repository's rows, or why they could not be read.
//
// The failure travels beside the rows rather than aborting the walk: one
// unreadable repository must not cost the others their place in a listing whose
// whole purpose is covering all of them.
type Repo struct {
	Root string
	// Err is why this repository could not be read, empty when it could. Rows is
	// nil rather than empty when it is set — "no worktrees" and "none readable"
	// are different facts.
	Err  string
	Rows []Row
}

// AllRows reads the supplied repositories, in the order given.
//
// The workspace list is fetched once and joined against every repository's
// worktrees: the agent status lives on the workspace, and asking herdr for the
// same global list once per repository would be N round trips for one answer.
//
// Which repositories to read is the caller's to decide. Discovery already
// exists — herdr's open worktree workspaces — and a second one here would be a
// second answer to which repositories this plugin manages.
func AllRows(client *herdrapi.Client, roots []string) ([]Repo, error) {
	if len(roots) == 0 {
		return nil, nil
	}

	workspaces, err := client.WorkspaceList()
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}

	repos := make([]Repo, 0, len(roots))
	for _, root := range roots {
		repo := Repo{Root: root}
		if worktrees, err := client.WorktreeList(root); err != nil {
			repo.Err = err.Error()
		} else {
			repo.Rows = Rows(worktrees, workspaces)
		}
		repos = append(repos, repo)
	}
	return repos, nil
}

// ListJSON is what `ls --json` writes. An object rather than a bare array, so a
// later field has somewhere to go that does not change the type of the document.
type ListJSON struct {
	// Worktrees is the listing for a single repository, and null when the answer
	// was grouped by repository instead.
	Worktrees []RowJSON `json:"worktrees"`
	// Repositories is the `--all-repos` listing, and null when a single
	// repository was listed. Exactly one of the two fields is ever non-null, so
	// a consumer can tell which question was asked from the document alone.
	//
	// Grouped, rather than a repository field on every row — which was the other
	// candidate and the cheaper one. Three things decided it:
	//
	// A per-repository failure needs somewhere to live. `--all-repos` keeps
	// going when one repository cannot be read, and a flat array of worktrees
	// has nowhere to record that except by inventing a row that is not a
	// worktree.
	//
	// A repository whose rows are all filtered out — `--blocked` is the usual
	// reason — has to survive as an empty group. "Asked, and none" versus "not
	// asked" is the distinction this format exists to keep, and a flat array
	// erases it by simply having fewer rows.
	//
	// And `doctor --json` already reports per repository, so the two
	// cross-repository views nest the same way rather than each inventing one.
	Repositories []RepoJSON `json:"repositories"`
}

// RepoJSON is one repository in a cross-repository listing.
type RepoJSON struct {
	// Root is the repository path; Name is the basename the table would show,
	// carried so a consumer does not have to re-derive the label.
	Root string `json:"root"`
	Name string `json:"name"`
	// Error is why this repository could not be read. Worktrees is null when it
	// is set, empty when the repository was read and nothing matched.
	Error     *string   `json:"error"`
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

// ReposJSON projects a cross-repository listing for a machine.
func ReposJSON(repos []Repo) []RepoJSON {
	out := make([]RepoJSON, 0, len(repos))
	for _, repo := range repos {
		entry := RepoJSON{Root: repo.Root, Name: filepath.Base(repo.Root), Error: jsonout.String(repo.Err)}
		if repo.Err == "" {
			entry.Worktrees = JSON(repo.Rows)
		}
		out = append(out, entry)
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
func Ls(client *herdrapi.Client, root, dir string, lookupPR func(branch string) (string, error), opts Options, out io.Writer) error {
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
	// Filtered before the lookups, not after: a pane costs a round trip, and a
	// row that will not be printed is not worth one.
	if opts.Blocked {
		rows = OnlyBlocked(rows)
	}
	WithPanes(client, rows)
	if lookupPR != nil {
		WithPRs(rows, lookupPR)
	}

	if opts.JSON {
		return jsonout.Write(out, ListJSON{Worktrees: JSON(rows)})
	}
	// An empty table is the same output as a listing that reached nothing, so a
	// filter that matched nothing says so.
	if opts.Blocked && len(rows) == 0 {
		fmt.Fprintln(out, "no blocked agents in "+safetext.Escape(root))
		return nil
	}
	return Render(out, rows, lookupPR != nil)
}

// LsAll lists every worktree of every supplied repository, grouped by
// repository. This is the view for someone with agents running in six
// checkouts, for whom the per-repository listing means six invocations and six
// directories to remember to visit.
//
// There is no pull request lookup here, and that is deliberate rather than
// pending. The lookup runs one `gh` call per branch in series, so across six
// repositories it is a listing nobody would wait for — and it is scoped to a
// single repository, so pointing it at another repository's branches would
// quietly ask the wrong GitHub repository and answer confidently. Pairing them
// needs concurrency and a per-repository lookup first.
func LsAll(client *herdrapi.Client, roots []string, opts Options, out io.Writer) error {
	repos, err := AllRows(client, roots)
	if err != nil {
		return err
	}

	for i := range repos {
		if opts.Blocked {
			repos[i].Rows = OnlyBlocked(repos[i].Rows)
		}
		WithPanes(client, repos[i].Rows)
	}

	if opts.JSON {
		return jsonout.Write(out, ListJSON{Repositories: ReposJSON(repos)})
	}
	return RenderRepos(out, repos, opts)
}

func deref(s *string) string {
	if s == nil || strings.TrimSpace(*s) == "" {
		return ""
	}
	return *s
}
