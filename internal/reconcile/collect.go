package reconcile

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/steig/muster/internal/gitx"
	"github.com/steig/muster/internal/herdrapi"
)

// Collector gathers the facts Reconcile needs. Every field is injectable so
// tests can substitute a fake herdr, a fake gh, and a temp HOME.
type Collector struct {
	Client *herdrapi.Client
	// Root is the repository's main checkout.
	Root string
	// ProjectsDir is where Claude Code keeps conversations, normally
	// ~/.claude/projects.
	ProjectsDir string
	// LookupPR resolves a branch's pull request state. Nil disables PR
	// lookups, which makes every verdict fall back to git.
	LookupPR func(branch string) PRState
}

// NewCollector builds a Collector with the default gh-backed PR lookup.
func NewCollector(client *herdrapi.Client, root string) *Collector {
	c := &Collector{
		Client:      client,
		Root:        root,
		ProjectsDir: DefaultProjectsDir(),
	}
	c.LookupPR = func(branch string) PRState { return GhPRState(root, branch) }
	return c
}

// DefaultProjectsDir is ~/.claude/projects.
func DefaultProjectsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// Collect reads git, herdr, gh, and the transcript directory into a State.
func (c *Collector) Collect() (State, error) {
	base := gitx.BaseRef(c.Root)

	worktrees, err := c.Client.WorktreeList(c.Root)
	if err != nil {
		return State{}, err
	}
	workspaces, err := c.Client.WorkspaceList()
	if err != nil {
		return State{}, err
	}
	agents, err := c.Client.AgentList()
	if err != nil {
		return State{}, err
	}

	state := State{Base: base, AgentPanes: map[string]bool{}}

	for _, a := range agents.Agents {
		state.AgentPanes[a.PaneID] = true
	}

	for _, w := range worktrees.Worktrees {
		branch := deref(w.Branch)
		wt := Worktree{
			Path:        w.Path,
			Branch:      branch,
			IsLinked:    w.IsLinkedWorktree,
			IsBare:      w.IsBare,
			WorkspaceID: deref(w.OpenWorkspaceID),
		}

		// Only linked worktrees are ever staffed or pruned, so the expensive
		// per-checkout git and gh work is skipped for the main checkout.
		if w.IsLinkedWorktree {
			wt.Dirty = gitx.IsDirty(w.Path)
			wt.OwnCommits = gitx.OwnCommits(w.Path, base)
			wt.HasTranscript = c.hasTranscript(w.Path)
			if branch != "" {
				wt.MergedIntoBase = gitx.IsMergedInto(c.Root, branch, base)
				if c.LookupPR != nil {
					wt.PR = c.LookupPR(branch)
				}
			}
		}
		state.Worktrees = append(state.Worktrees, wt)
	}

	for _, ws := range workspaces.Workspaces {
		// herdr reports resolved paths; c.Root may not be. Compare normalised
		// or every workspace is filtered out and sync becomes a silent no-op.
		if ws.Worktree == nil || gitx.Resolve(ws.Worktree.RepoRoot) != gitx.Resolve(c.Root) {
			continue
		}
		panes, err := c.Client.PaneList(ws.WorkspaceID)
		if err != nil {
			return State{}, err
		}

		ids := make([]string, 0, len(panes.Panes))
		for _, p := range panes.Panes {
			ids = append(ids, p.PaneID)
		}
		state.Workspaces = append(state.Workspaces, Workspace{
			ID:           ws.WorkspaceID,
			CheckoutPath: ws.Worktree.CheckoutPath,
			IsLinked:     ws.Worktree.IsLinkedWorktree,
			PaneIDs:      ids,
		})
	}

	return state, nil
}

// hasTranscript reports whether Claude Code has a stored conversation for the
// checkout — a directory named for the path holding at least one *.jsonl.
func (c *Collector) hasTranscript(checkout string) bool {
	if c.ProjectsDir == "" {
		return false
	}
	matches, err := filepath.Glob(filepath.Join(c.ProjectsDir, TranscriptSlug(checkout), "*.jsonl"))
	return err == nil && len(matches) > 0
}

// GhPRState asks gh for a branch's pull request state. A missing gh, a missing
// PR, or any error yields PRNone, which leaves the verdict to git.
func GhPRState(root, branch string) PRState {
	args := []string{"pr", "view", branch, "--json", "state"}
	if origin := gitx.RemoteURL(root); origin != "" {
		args = append(args, "--repo", origin)
	}

	cmd := exec.Command("gh", args...)
	cmd.Dir = root

	out, err := cmd.Output()
	if err != nil {
		return PRNone
	}

	var payload struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return PRNone
	}

	switch PRState(payload.State) {
	case PRMerged:
		return PRMerged
	case PRClosed:
		return PRClosed
	case PROpen:
		return PROpen
	default:
		return PRNone
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
