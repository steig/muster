package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/steig/worktender/internal/herdrapi"
	"github.com/steig/worktender/internal/safetext"
	"github.com/steig/worktender/internal/wt"
)

// doctor answers "what is wrong", for the failure modes that do not announce
// themselves.
//
// Four of this plugin's documented failures are environmental, silent, and
// shaped exactly like ordinary operation. An unauthenticated `gh` collapses to
// "no pull request", so prune keeps everything and every printed reason looks
// reasonable. An unrecognised WORKTENDER_EVENTS leaves events off, and the
// notice that would say so is printed by a hook that no longer fires. A stale
// remote-tracking ref reads as an upstream still present. An install left four
// releases behind reads as an install. Each was previously diagnosed by
// remembering it exists.
//
// It is read-only and takes no lock, and it must work from outside a
// repository: someone who cannot tell what is wrong often cannot tell where
// they are either. So it asks herdr for the repositories rather than asking git
// where it is standing.
func doctorCommand(out io.Writer) error {
	client, clientErr := herdrapi.New()
	herdr := check{name: "herdr", value: "unreachable", state: stateFail}
	if clientErr == nil {
		herdr = herdrReachable(client)
	} else {
		herdr.note = clientErr.Error()
	}

	checks := []check{versionCheck(client), herdr, ghCheck(), eventsCheck()}

	// Rendered through a buffer so the padding of a check with no note does not
	// leave a line of trailing spaces.
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

	// Nothing below this line is answerable without herdr, and saying so once is
	// better than four repetitions of the same cause.
	if clientErr != nil {
		return fmt.Errorf("cannot reach herdr, so nothing else here could be checked: %w", clientErr)
	}

	return reportRepositories(client, out)
}

// state is a check's verdict. It is deliberately not a boolean: "off" is the
// documented default for events and is not a problem, while "warn" is a real
// capability the user has lost without being told.
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

// versionCheck reports what is installed and whether anything newer exists.
//
// This is the fourth silent failure, and the one that hides longest. An install
// pins a commit and stays on it: herdr has no `plugin update`, so moving forward
// is something you have to know to do, and nothing in ordinary use ever mentions
// it. One install sat on 8ef0de9 across four releases while `doctor` itself, the
// --permission-mode passthrough and the docs split all landed.
func versionCheck(client *herdrapi.Client) check {
	root, err := installRoot()
	if err != nil {
		return check{name: "version", value: "unknown", state: stateWarn, note: err.Error()}
	}
	return installCheck(client, root)
}

// installCheck is versionCheck against an explicit install root.
//
// It reports TWO drifts, because they are different and both silent. Behind
// origin is the ordinary one. herdr recording a commit that is not on disk is
// the one an update creates: herdr records the commit at install time and never
// re-reads the checkout, so after any in-place update `plugin list` — the one
// command that answers "what am I running" — answers with a commit that is gone.
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

// herdrReachable reports the running herdr's version when it can be had.
//
// The version comes from the CLI rather than the socket because the socket does
// not carry one, and it matters: every measured behaviour behind `report` and
// `gate` was verified against 0.7.5. A version that cannot be read is reported
// as unknown rather than assumed current — the manifest's min_herdr_version is
// what actually enforces the floor, at install time.
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

// ghCheck is the one worth running this command for.
//
// Every `gh` failure — missing, or installed but not authenticated — collapses
// to "this branch has no pull request", which resolves to keep. So a repository
// that uses pull requests prunes almost nothing while every reason it prints
// reads as ordinary. This is a warning rather than a failure: a repository that
// does not use pull requests is entitled to no `gh` at all.
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
		// The gate trims before deciding, so a stray `export WORKTENDER_EVENTS=" "`
		// is off for the same reason unset is. Printing the raw value here would
		// render as an empty column and read as a bug in this command.
		return check{name: "events", value: "unset", state: stateOff}
	default:
		return check{name: "events", value: raw, state: stateOff}
	}
}

// reportRepositories lists what herdr currently holds, one line per repository.
//
// The scope is herdr's open worktree workspaces rather than the caller's
// directory, for the same reason startup uses it: this has to answer from
// anywhere, including from outside any repository at all.
func reportRepositories(client *herdrapi.Client, out io.Writer) error {
	roots, err := openRepositories(client)
	if err != nil {
		return fmt.Errorf("list workspaces: %w", err)
	}
	if len(roots) == 0 {
		fmt.Fprintln(out, "\nno repositories: herdr has no worktree workspaces open")
		return nil
	}

	workspaces, err := client.WorkspaceList()
	if err != nil {
		return fmt.Errorf("list workspaces: %w", err)
	}

	fmt.Fprintln(out, "\nrepos")
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, root := range roots {
		worktrees, err := client.WorktreeList(root)
		if err != nil {
			// One unreadable repository must not cost the others their line.
			fmt.Fprintf(tw, "  %s\t\t%s\n", safetext.Escape(filepath.Base(root)), safetext.Escape(err.Error()))
			continue
		}
		rows := wt.Rows(worktrees, workspaces)
		fmt.Fprintf(tw, "  %s\t%s\t%s\n",
			safetext.Escape(filepath.Base(root)), plural(len(rows), "worktree"), summariseAgents(rows))
	}
	return tw.Flush()
}

// summariseAgents counts the agent statuses herdr reports, busiest first so the
// interesting half of the line comes first.
func summariseAgents(rows []wt.Row) string {
	counts := map[string]int{}
	for _, r := range rows {
		if r.AgentStatus == "" || r.AgentStatus == "-" {
			continue
		}
		counts[r.AgentStatus]++
	}
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
