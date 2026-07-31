package main

import (
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/steig/worktender/internal/herdrapi"
)

// The manifest and dispatch are two lists of event kinds maintained by hand in
// two files, and nothing made them agree. Both ways of disagreeing cost:
//
//   - Subscribed, unhandled. herdr spawns a process per event, which parses the
//     envelope, prints "ignoring", and exits 0. The manifest declined to
//     subscribe to pane.agent_status_changed on exactly this cost argument while
//     carrying pane.agent_detected, which had no handler.
//   - Handled, unsubscribed. The handler is unreachable — herdr delivers only
//     what the manifest asks for — so the behaviour it implements never runs and
//     nothing says so.
//
// The trap in writing this test is that the two lists are in DIFFERENT
// NAMESPACES. `on =` takes herdr's dotted manifest name ("pane.agent_detected");
// the payload discriminator EventKind is generated from is underscored
// ("pane_agent_detected"). Compare them as they are written and every
// subscription looks unhandled — the test passes the day someone deletes the
// last subscription and fails forever otherwise. eventKindOf does the crossing,
// and TestManifestEventNamesAreNotDiscriminators pins the gap so a later
// "simplification" back to a direct comparison fails loudly.

// manifestEventSubscriptions returns the `on =` value of every [[events]] block
// in the manifest, in the manifest's own dotted namespace.
//
// Hand-scanned rather than parsed: this module has no dependencies at all, and
// a TOML library is a poor trade for reading one key out of one table.
func manifestEventSubscriptions(t *testing.T) []string {
	t.Helper()

	raw, err := os.ReadFile("herdr-plugin.toml")
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}

	var subscriptions []string
	inEvents := false
	for line := range strings.SplitSeq(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			// Comments name event kinds too — including the one this plugin
			// deliberately does NOT subscribe to.
			continue
		}
		if strings.HasPrefix(line, "[") {
			inEvents = line == "[[events]]"
			continue
		}
		if !inEvents {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "on" {
			continue
		}
		on, err := unquoteManifestValue(strings.TrimSpace(value))
		if err != nil {
			t.Fatalf("manifest `on` is not a quoted string: %s", line)
		}
		subscriptions = append(subscriptions, on)
	}

	if len(subscriptions) == 0 {
		// Every assertion below is vacuous on an empty list, so a scanner that
		// quietly stopped matching would read as a clean bill of health.
		t.Fatal("found no [[events]] subscriptions in the manifest; the scanner is broken or the hooks are gone")
	}
	return subscriptions
}

// unquoteManifestValue reads a double-quoted TOML string, so a bare or
// single-quoted token is a test failure rather than a silent match.
func unquoteManifestValue(value string) (string, error) {
	var out string
	if err := json.Unmarshal([]byte(value), &out); err != nil {
		return "", err
	}
	return out, nil
}

// eventKindOf crosses from the manifest's dotted name to the payload
// discriminator. Exactly one dot, and only the first is crossed: the category is
// dot-separated and the remainder is not, so "pane.agent_detected" is one dot
// and one underscore, both load-bearing.
func eventKindOf(t *testing.T, on string) herdrapi.EventKind {
	t.Helper()

	if strings.Count(on, ".") != 1 {
		t.Fatalf("manifest event %q has %d dots; the dotted-to-underscored crossing assumes exactly one", on, strings.Count(on, "."))
	}
	return herdrapi.EventKind(strings.Replace(on, ".", "_", 1))
}

// Every event the manifest subscribes to must reach a handler. A subscription
// without one is a process spawned per event to print "ignoring" and exit.
func TestEveryManifestEventHasAHandler(t *testing.T) {
	for _, on := range manifestEventSubscriptions(t) {
		kind := eventKindOf(t, on)

		// Through real dispatch, not against the set: a minimal envelope of this
		// kind must not come back unhandled.
		envelope := herdrapi.EventEnvelope{
			Event: kind,
			Data:  []byte(`{"type":"` + string(kind) + `"}`),
		}
		if _, err := envelope.Scope(); errors.Is(err, herdrapi.ErrUnhandledEvent) {
			t.Errorf("manifest subscribes to %q (%s) and nothing handles it; every firing spawns a process to do nothing", on, kind)
		}
	}
}

// And the converse. herdr delivers only what the manifest asks for, so a handler
// with no subscription never runs.
func TestEveryHandledEventIsSubscribed(t *testing.T) {
	var subscribed []herdrapi.EventKind
	for _, on := range manifestEventSubscriptions(t) {
		subscribed = append(subscribed, eventKindOf(t, on))
	}

	for _, kind := range herdrapi.HandledEventKinds() {
		if !slices.Contains(subscribed, kind) {
			t.Errorf("%s has a handler but no [[events]] subscription; herdr will never deliver it", kind)
		}
	}
}

// The namespace gap itself, pinned. If the manifest's own spelling ever started
// matching a discriminator, the crossing above would look like dead code and the
// next reader would delete it — at which point the two tests above would compare
// dotted names against underscored ones and agree that nothing is handled.
func TestManifestEventNamesAreNotDiscriminators(t *testing.T) {
	for _, on := range manifestEventSubscriptions(t) {
		if slices.Contains(herdrapi.HandledEventKinds(), herdrapi.EventKind(on)) {
			t.Errorf("manifest name %q matched a payload discriminator verbatim; the two namespaces are supposed to differ", on)
		}
	}
}
