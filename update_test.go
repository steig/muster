package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steig/worktender/internal/herdrtest"
)

// fakeBuild stands in for scripts/build.sh. It honours WORKTENDER_BUILD_OUT the
// way the real script does, writes the version it built so the live binary can
// be identified afterwards, and records the path it was ASKED for — which is
// what proves the build never targets the file it is replacing.
const fakeBuild = `#!/bin/sh
set -eu
out="${WORKTENDER_BUILD_OUT:-bin/worktender}"
mkdir -p "$(dirname "$out")"
printf '%s\n' "$out" > bin/asked-for
awk -F'"' '/^version/ {print $2}' herdr-plugin.toml > "$out"
chmod +x "$out"
`

// newInstall builds what herdr's installer leaves on disk: a shallow, DETACHED
// clone with no local branch, plus a binary at the path an update replaces.
//
// The shape is the point. `git pull` cannot work in it, which is why update
// fetches and resets instead, and a test against an ordinary clone on a branch
// would never exercise that.
func newInstall(t *testing.T) (*herdrtest.Repo, string) {
	t.Helper()

	origin := herdrtest.NewRepo(t)
	publish(t, origin, "0.1.0")

	parent := t.TempDir()
	root := filepath.Join(parent, "steig.worktender-3ebd1704d63b")
	// file:// rather than a plain path: git ignores --depth on a local clone.
	origin.GitIn(parent, "clone", "--depth", "1", "file://"+origin.RealRoot, root)
	origin.GitIn(root, "checkout", "--detach", "HEAD")

	herdrtest.WriteFile(t, filepath.Join(root, "bin", binaryName()), "0.1.0\n")
	return origin, root
}

// publish commits a release to the origin: the manifest version update reads,
// and the build script it runs.
func publish(t *testing.T, origin *herdrtest.Repo, version string) {
	t.Helper()
	publishBuild(t, origin, version, fakeBuild)
}

// publishBuild is publish with the build script named, because the script comes
// from the checkout being fetched and update does not get to assume anything
// about it.
func publishBuild(t *testing.T, origin *herdrtest.Repo, version, script string) {
	t.Helper()

	// bin/ is ignored in the real repository, which is what keeps an install
	// with a built binary in it clean enough to reset over.
	origin.Write(".gitignore", "bin/\n")
	origin.Write(manifestName, "id = \"steig.worktender\"\nversion = \""+version+"\"\n")
	origin.Write("scripts/build.sh", script)
	origin.Git("add", ".")
	origin.Git("commit", "-m", "release "+version)
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.TrimSpace(string(raw))
}

func TestUpdateFetchesResetsAndRebuilds(t *testing.T) {
	origin, root := newInstall(t)
	was := origin.GitIn(root, "rev-parse", "HEAD")
	publish(t, origin, "0.2.0")

	var out strings.Builder
	if err := update(root, &out); err != nil {
		t.Fatalf("update: %v\n%s", err, out.String())
	}

	if got := readFile(t, filepath.Join(root, "bin", binaryName())); got != "0.2.0" {
		t.Errorf("the live binary is the %q build; the rebuilt one never landed", got)
	}
	if version, err := manifestVersion(root); err != nil || version != "0.2.0" {
		t.Errorf("manifest version = %q (%v), want 0.2.0", version, err)
	}
	// Both commits, because "updated" with only the new one is unverifiable.
	for _, want := range []string{short(was), "0.1.0", "0.2.0"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output should name %q, got:\n%s", want, out.String())
		}
	}
}

// The failure the issue was written from: an update rebuilds a binary herdr may
// be executing, and is normally run BY that binary. The build must therefore be
// asked for somewhere other than the live path, and moved into place afterwards.
func TestUpdateNeverBuildsOverTheRunningBinary(t *testing.T) {
	origin, root := newInstall(t)
	publish(t, origin, "0.2.0")

	var out strings.Builder
	if err := update(root, &out); err != nil {
		t.Fatalf("update: %v\n%s", err, out.String())
	}

	live := filepath.Join("bin", binaryName())
	if asked := readFile(t, filepath.Join(root, "bin", "asked-for")); asked == live {
		t.Errorf("the build was pointed at %s, the file it is replacing", asked)
	}
}

// A build script that ignores WORKTENDER_BUILD_OUT writes the live path anyway —
// every release before this one does, and update fetches its build script from
// the checkout it just pulled. The staging guarantee is lost when that happens
// and cannot be recovered; what must not also be lost is an accurate account,
// because "nothing was staged" reads as "nothing was built" while the running
// binary has in fact already been replaced.
func TestUpdateReportsWhatTheBuildDidToTheLiveBinary(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
		says   string
		binary string
	}{
		{"one that ignores the staging path", ignoringBuild, "ignored " + buildOutEnv, "0.2.0"},
		{"one that builds nothing at all", silentBuild, "left", "0.1.0"},
		{"one that fails without touching it", failingBuild, "untouched", "0.1.0"},
		{"one that fails after writing it", clobberingBuild, "written over anyway", "half"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			origin, root := newInstall(t)
			publishBuild(t, origin, "0.2.0", tc.script)

			var out strings.Builder
			err := update(root, &out)
			if err == nil {
				t.Fatalf("a rebuild that staged nothing must fail; output was:\n%s", out.String())
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("the failure should say %q, got %v", tc.says, err)
			}
			// The claim under test is the one about the binary on disk, so it is
			// checked against the binary on disk.
			if got := readFile(t, filepath.Join(root, "bin", binaryName())); got != tc.binary {
				t.Errorf("the live binary is %q, and the failure describes it as %q", got, tc.binary)
			}
		})
	}
}

// The build script as it was before WORKTENDER_BUILD_OUT existed.
const ignoringBuild = `#!/bin/sh
set -eu
awk -F'"' '/^version/ {print $2}' herdr-plugin.toml > bin/worktender
chmod +x bin/worktender
`

const silentBuild = `#!/bin/sh
exit 0
`

const failingBuild = `#!/bin/sh
echo "no toolchain" >&2
exit 1
`

// The worst shape: it writes the live binary and then fails, so what is on disk
// is neither the old build nor a finished one.
const clobberingBuild = `#!/bin/sh
set -eu
printf 'half\n' > bin/worktender
exit 1
`

// The other half nothing can fix: herdr records the installed commit once and
// never re-reads the checkout, so `plugin list` keeps naming a commit that an
// in-place update has replaced. The command has to say so.
func TestUpdateSaysHerdrWillKeepReportingTheOldCommit(t *testing.T) {
	origin, root := newInstall(t)
	was := origin.GitIn(root, "rev-parse", "HEAD")
	publish(t, origin, "0.2.0")

	server := herdrtest.NewServer(t)
	t.Setenv("HERDR_SOCKET_PATH", server.SocketPath)
	server.HandleResult("plugin.list", pluginListReply(root, was))

	var out strings.Builder
	if err := update(root, &out); err != nil {
		t.Fatalf("update: %v\n%s", err, out.String())
	}

	for _, want := range []string{short(was), "plugin list"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output should warn about %q, got:\n%s", want, out.String())
		}
	}
}

// pluginListReply is herdr's answer about one installed plugin, carrying the
// commit it recorded at install time.
func pluginListReply(root, commit string) map[string]any {
	return map[string]any{"type": "plugin_list", "plugins": []map[string]any{{
		"plugin_id": "steig.worktender", "name": "worktender", "version": "0.1.0",
		"enabled": true, "plugin_root": root, "manifest_path": filepath.Join(root, manifestName),
		"source": map[string]any{"kind": "github", "owner": "steig", "repo": "worktender",
			"resolved_commit": commit},
	}}}
}

// Both refusals guard the same mistake: this only knows how to move the
// detached checkout herdr installs, and `git reset --hard` anywhere else
// destroys work that exists nowhere but there.
func TestUpdateRefusesAnythingButAManagedInstall(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, origin *herdrtest.Repo, root string)
		says  string
	}{
		{"a checkout on a branch", func(t *testing.T, origin *herdrtest.Repo, root string) {
			origin.GitIn(root, "checkout", "-b", "work")
		}, "branch work"},
		{"uncommitted work", func(t *testing.T, origin *herdrtest.Repo, root string) {
			herdrtest.WriteFile(t, filepath.Join(root, "scratch.txt"), "wip\n")
		}, "uncommitted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			origin, root := newInstall(t)
			was := origin.GitIn(root, "rev-parse", "HEAD")
			publish(t, origin, "0.2.0")
			tc.setup(t, origin, root)

			var out strings.Builder
			err := update(root, &out)
			if err == nil {
				t.Fatalf("update should have refused; output was:\n%s", out.String())
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("the refusal should say %q, got %v", tc.says, err)
			}
			if now := origin.GitIn(root, "rev-parse", "HEAD"); now != was {
				t.Errorf("the refusal still moved HEAD from %s to %s", short(was), short(now))
			}
		})
	}
}

// Nothing to fetch must mean nothing rebuilt. A rebuild that ran anyway would
// swap the binary herdr is executing for no reason at all.
func TestUpdateRebuildsNothingWhenAlreadyCurrent(t *testing.T) {
	_, root := newInstall(t)

	var out strings.Builder
	if err := update(root, &out); err != nil {
		t.Fatalf("update: %v\n%s", err, out.String())
	}

	if !strings.Contains(out.String(), "already current") {
		t.Errorf("output should say the install is current, got:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(root, "bin", "asked-for")); err == nil {
		t.Error("the build ran on an install that had nothing to fetch")
	}
}

func TestManifestVersion(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name     string
		manifest string
		want     string
	}{
		{"a plain version", "id = \"x\"\nversion = \"0.5.0\"\n", "0.5.0"},
		// min_herdr_version ends in the same word and must not be mistaken for it.
		{"the herdr floor first", "min_herdr_version = \"0.7.5\"\nversion = \"0.5.0\"\n", "0.5.0"},
		{"no version at all", "id = \"x\"\n", ""},
		{"an unquoted version", "version = 0.5.0\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			herdrtest.WriteFile(t, filepath.Join(dir, manifestName), tc.manifest)

			got, err := manifestVersion(dir)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("manifestVersion = %q, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("manifestVersion: %v", err)
			}
			if got != tc.want {
				t.Errorf("manifestVersion = %q, want %q", got, tc.want)
			}
		})
	}
}
