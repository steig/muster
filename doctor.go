package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/steig/worktender/internal/gitx"
	"github.com/steig/worktender/internal/herdrapi"
	"github.com/steig/worktender/internal/jsonout"
	"github.com/steig/worktender/internal/safetext"
	"github.com/steig/worktender/internal/wt"
)

// doctor answers "what is wrong", for the failure modes that do not announce
// themselves: an unauthenticated `gh` collapsing to "no pull request", an
// unrecognised WORKTENDER_EVENTS leaving events off, a stale remote-tracking
// ref reading as an upstream still present, an install left releases behind.
//
// It is read-only, takes no lock, and works from outside a repository — so it
// asks herdr for the repositories rather than asking git where it is standing.
func doctorCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := jsonFlag(fs)

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%v; %s", err, doctorUsage)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q; %s", fs.Arg(0), doctorUsage)
	}

	client, clientErr := herdrapi.New()
	herdr := check{name: "herdr", value: "unreachable", state: stateFail}
	if clientErr == nil {
		herdr = herdrReachable(client)
	} else {
		herdr.note = clientErr.Error()
	}

	checks := []check{versionCheck(client), herdr, ghCheck(), eventsCheck()}

	// Nothing below the checks is answerable without herdr, and saying so once
	// is better than four repetitions of the same cause.
	var fatal error
	var repos []repoSummary
	if clientErr != nil {
		fatal = fmt.Errorf("cannot reach herdr, so nothing else here could be checked: %w", clientErr)
	} else if repos, fatal = repositories(client); fatal != nil {
		repos = nil
	}

	if *asJSON {
		// The failure is in the document as well as on stderr: a consumer that
		// only reads stdout would otherwise see checks and an empty repository
		// list, which is what a healthy herdr with nothing open also looks like.
		if err := writeDoctorJSON(out, checks, repos, fatal); err != nil {
			return err
		}
		return fatal
	}

	if err := renderChecks(out, checks); err != nil {
		return err
	}
	fmt.Fprint(out, binaryLine())
	if fatal != nil {
		return fatal
	}
	return renderRepositories(out, repos)
}

const doctorUsage = "usage: worktender doctor [--json]"

// renderChecks writes the check table.
//
// It goes through a buffer so the padding of a check with no note does not
// leave a line of trailing spaces.
func renderChecks(out io.Writer, checks []check) error {
	var table bytes.Buffer
	tw := tabwriter.NewWriter(&table, 0, 0, 2, ' ', 0)
	for _, c := range checks {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", c.name, safetext.Escape(c.value), c.state, safetext.Escape(c.note))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for line := range strings.SplitSeq(strings.TrimRight(table.String(), "\n"), "\n") {
		fmt.Fprintln(out, strings.TrimRight(line, " "))
	}
	return nil
}

// doctorJSON is what `doctor --json` writes. The shape may move before 1.0.
type doctorJSON struct {
	Checks []checkJSON `json:"checks"`
	// Binary is this process's own path, null when it cannot say where it
	// lives. It is what the text mode's "run it from a shell with" line carries.
	Binary *string `json:"binary"`
	// Repositories is null rather than empty when Error is set: an empty list
	// is the answer for a herdr with nothing open, which is a different fact.
	Repositories []repoJSON `json:"repositories"`
	Error        *string    `json:"error"`
}

type checkJSON struct {
	Name  string  `json:"name"`
	Value string  `json:"value"`
	State string  `json:"state"`
	Note  *string `json:"note"`
}

// repoJSON is one repository at the granularity the table reports it: how many
// worktrees, and how many agents in each state. `ls --json` is the per-worktree
// view, and needs a repository to be run in.
type repoJSON struct {
	// Root is the repository path, which the table shows only the basename of.
	Root string `json:"root"`
	Name string `json:"name"`
	// Error is why this repository could not be read; the two counts below are
	// null when it is set, and one unreadable repository costs the others
	// nothing.
	Error     *string        `json:"error"`
	Worktrees *int           `json:"worktrees"`
	Agents    map[string]int `json:"agents"`
}

func writeDoctorJSON(out io.Writer, checks []check, repos []repoSummary, fatal error) error {
	document := doctorJSON{Checks: make([]checkJSON, 0, len(checks)), Binary: jsonout.String(binaryPath())}
	for _, c := range checks {
		document.Checks = append(document.Checks, checkJSON{
			Name: c.name, Value: c.value, State: string(c.state), Note: jsonout.String(c.note),
		})
	}

	if fatal != nil {
		document.Error = jsonout.String(fatal.Error())
	} else {
		document.Repositories = make([]repoJSON, 0, len(repos))
		for _, r := range repos {
			entry := repoJSON{Root: r.Root, Name: filepath.Base(r.Root), Error: jsonout.String(r.Err)}
			if r.Err == "" {
				count := len(r.Rows)
				entry.Worktrees = &count
				entry.Agents = agentCounts(r.Rows)
			}
			document.Repositories = append(document.Repositories, entry)
		}
	}
	return jsonout.Write(out, document)
}

// binaryLine is how to reach this binary from a shell, or "" when this process
// cannot say where it lives. The documented alternative is a jq expression over
// `herdr plugin list --json`, and this process already knows its own path.
//
// It is printed before the herdr check gates the rest: someone whose herdr is
// not answering still has a binary to run.
func binaryLine() string {
	path := binaryPath()
	if path == "" {
		return ""
	}
	return fmt.Sprintf("\nrun it from a shell with:\n  worktender=%s\n", safetext.Escape(path))
}

func binaryPath() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return gitx.Resolve(exe)
}

// state is a check's verdict, not a boolean: "off" is the documented default
// for events and is not a problem, while "warn" is a capability the user has
// lost without being told.
type state string

const (
	stateOK   state = "ok"
	stateOff  state = "off"
	stateWarn state = "warn"
	stateFail state = "fail"
)

type check struct {
	name  string
	value string
	state state
	note  string
}

// versionCheck reports what is installed and whether anything newer exists. An
// install pins a commit and stays on it — herdr has no `plugin update`, so
// nothing in ordinary use ever mentions moving forward.
func versionCheck(client *herdrapi.Client) check {
	root, err := installRoot()
	if err != nil {
		return check{name: "version", value: "unknown", state: stateWarn, note: err.Error()}
	}
	return installCheck(client, root)
}

// installCheck is versionCheck against an explicit install root. It reports two
// drifts: behind origin, and herdr recording a commit that is not on disk —
// herdr records the commit at install time and never re-reads the checkout, so
// after any in-place update `plugin list` names a commit that is gone.
func installCheck(client *herdrapi.Client, root string) check {
	version, err := manifestVersion(root)
	if err != nil {
		return check{name: "version", value: "unknown", state: stateWarn, note: err.Error()}
	}
	head, err := gitIn(root, "rev-parse", "HEAD")
	if err != nil {
		return check{name: "version", value: version, state: stateWarn,
			note: "not a git checkout, so nothing can be compared against origin"}
	}

	c := check{name: "version", value: version + " @" + short(head), state: stateOK}

	// A linked development checkout is on a branch and is the developer's to
	// move. Comparing it against origin would report work in progress as drift.
	if branch, err := gitIn(root, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
		c.note = "linked checkout on " + branch + ", so it is not compared against origin"
		return c
	}

	var notes []string
	if recorded := recordedCommitVia(client, root); recorded != "" && recorded != head {
		notes = append(notes, fmt.Sprintf("herdr records @%s, so `plugin list` names a commit that is not installed", short(recorded)))
	}
	switch branch, commit, err := remoteHead(root); {
	case err != nil:
		notes = append(notes, "origin could not be asked, so drift is unknown")
	case commit != head:
		notes = append(notes, fmt.Sprintf("origin/%s is at @%s; run `worktender update`", branch, short(commit)))
	}
	if len(notes) > 0 {
		c.state = stateWarn
		c.note = strings.Join(notes, "; ")
	}
	return c
}

// herdrReachable reports the running herdr's version when it can be had. It
// comes from the CLI because the socket does not carry one, and a version that
// cannot be read is reported as unknown rather than assumed current — the
// manifest's min_herdr_version enforces the floor at install time.
func herdrReachable(client *herdrapi.Client) check {
	if _, err := client.WorkspaceList(); err != nil {
		return check{name: "herdr", value: "not answering", state: stateFail, note: err.Error()}
	}
	v := herdrVersion()
	if v == "" {
		return check{name: "herdr", value: "reachable", state: stateOK, note: "version unknown"}
	}
	return check{name: "herdr", value: v, state: stateOK}
}

func herdrVersion() string {
	out, err := exec.Command("herdr", "--version").Output()
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	// `herdr 0.7.5` and a bare `0.7.5` are both plausible; keep the last field.
	if fields := strings.Fields(line); len(fields) > 0 {
		return fields[len(fields)-1]
	}
	return ""
}

// ghCheck is the one worth running this command for. Every `gh` failure —
// missing, or installed but not authenticated — collapses to "this branch has
// no pull request", which resolves to keep, while every printed reason reads as
// ordinary. A warning rather than a failure: a repository that does not use
// pull requests is entitled to no `gh` at all.
func ghCheck() check {
	if _, err := exec.LookPath("gh"); err != nil {
		return check{name: "gh", value: "not installed", state: stateWarn,
			note: "prune can only remove what a deleted upstream authorises"}
	}
	if err := exec.Command("gh", "auth", "status").Run(); err != nil {
		return check{name: "gh", value: "not authenticated", state: stateWarn,
			note: "reads as \"no pull request\", so prune will keep almost everything"}
	}
	return check{name: "gh", value: "authenticated", state: stateOK}
}

// eventsCheck reports the opt-in as the gate actually parses it, not as it is
// spelled. An unrecognised value is the case this exists for: it leaves events
// off, and the notice saying so is printed by a hook that will not fire.
func eventsCheck() check {
	raw := os.Getenv(eventsEnv)
	on, recognised := parseEventsValue(raw)

	switch {
	case !recognised:
		return check{name: "events", value: fmt.Sprintf("%q", raw), state: stateWarn,
			note: "not a value this gate recognises, so events stay off"}
	case on:
		return check{name: "events", value: "on", state: stateOK,
			note: "worktrees are adopted and staffed without being asked"}
	case strings.TrimSpace(raw) == "":
		// The gate trims before deciding, so whitespace is off for the same
		// reason unset is; printing the raw value would render as an empty column.
		return check{name: "events", value: "unset", state: stateOff}
	default:
		return check{name: "events", value: raw, state: stateOff}
	}
}

// repoSummary is one repository as doctor knows it: what herdr holds, or why it
// could not be read. Collected once and rendered either way, so the table and
// the document cannot describe different sessions.
type repoSummary struct {
	Root string
	// Err is why this repository could not be read, empty when it could. One
	// unreadable repository must not cost the others their place.
	Err  string
	Rows []wt.Row
}

// repositories reads what herdr currently holds. The scope is herdr's open
// worktree workspaces rather than the caller's directory, because doctor has to
// answer from outside any repository at all.
func repositories(client *herdrapi.Client) ([]repoSummary, error) {
	roots, err := openRepositories(client)
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	if len(roots) == 0 {
		return nil, nil
	}

	workspaces, err := client.WorkspaceList()
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}

	summaries := make([]repoSummary, 0, len(roots))
	for _, root := range roots {
		summary := repoSummary{Root: root}
		if worktrees, err := client.WorktreeList(root); err != nil {
			summary.Err = err.Error()
		} else {
			summary.Rows = wt.Rows(worktrees, workspaces)
		}
		summaries = append(summaries, summary)
	}
	return summaries, nil
}

// renderRepositories lists what herdr holds, one line per repository.
func renderRepositories(out io.Writer, summaries []repoSummary) error {
	if len(summaries) == 0 {
		fmt.Fprintln(out, "\nno repositories: herdr has no worktree workspaces open")
		return nil
	}

	fmt.Fprintln(out, "\nrepos")
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, summary := range summaries {
		name := safetext.Escape(filepath.Base(summary.Root))
		if summary.Err != "" {
			fmt.Fprintf(tw, "  %s\t\t%s\n", name, safetext.Escape(summary.Err))
			continue
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n",
			name, plural(len(summary.Rows), "worktree"), summariseAgents(summary.Rows))
	}
	return tw.Flush()
}

// agentCounts tallies the agent statuses herdr reports for these worktrees.
func agentCounts(rows []wt.Row) map[string]int {
	counts := map[string]int{}
	for _, r := range rows {
		if r.AgentStatus == "" {
			continue
		}
		counts[r.AgentStatus]++
	}
	return counts
}

// summariseAgents renders those counts busiest first, so the interesting half
// of the line comes first.
func summariseAgents(rows []wt.Row) string {
	counts := agentCounts(rows)
	if len(counts) == 0 {
		return "no agents"
	}

	statuses := make([]string, 0, len(counts))
	for s := range counts {
		statuses = append(statuses, s)
	}
	sort.Slice(statuses, func(i, j int) bool {
		if counts[statuses[i]] != counts[statuses[j]] {
			return counts[statuses[i]] > counts[statuses[j]]
		}
		return statuses[i] < statuses[j]
	})

	parts := make([]string, 0, len(statuses))
	for _, s := range statuses {
		parts = append(parts, fmt.Sprintf("%d %s", counts[s], safetext.Escape(s)))
	}
	return strings.Join(parts, ", ")
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
