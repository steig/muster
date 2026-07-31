package herdrapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
)

// EventEnv is the environment variable herdr sets to the event envelope when it
// invokes an [[events]] hook.
//
// herdr also sets HERDR_PLUGIN_EVENT, to the DOTTED manifest name
// ("worktree.opened"), while the envelope's own `event` field carries the
// UNDERSCORED payload discriminator ("worktree_opened"). Both arrive in the same
// invocation. Dispatch reads the envelope, because those are the names EventKind
// is generated from; matching against the dotted name would compile and never
// fire.
const EventEnv = "HERDR_PLUGIN_EVENT_JSON"

// ErrNoEvent reports that herdr injected no event, which means the process was
// not started by herdr as an event hook.
var ErrNoEvent = errors.New(EventEnv + " is not set; not running as a herdr event hook")

// ErrUnhandledEvent reports an event this plugin subscribes to but has no
// behaviour for. It is not a failure: the manifest and the binary can differ
// across an upgrade, and a hook firing into no handler is a no-op, not a bug.
var ErrUnhandledEvent = errors.New("no handler for this event kind")

// LoadEvent reads the event envelope herdr injected.
//
// A malformed envelope is fatal for the same reason a malformed invocation
// context is: it means herdr sent something unparseable, which is a bug to
// surface rather than a state to default around.
func LoadEvent() (EventEnvelope, error) {
	raw := os.Getenv(EventEnv)
	if raw == "" {
		return EventEnvelope{}, ErrNoEvent
	}

	var envelope EventEnvelope
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return EventEnvelope{}, fmt.Errorf("malformed %s: %w", EventEnv, err)
	}
	return envelope, nil
}

// Scope is the repository an event is about.
type Scope struct {
	// RepoRoot is the main checkout of the repository to reconcile.
	RepoRoot string
	// Checkout is the worktree the event concerned, for the log line. It is
	// never used to decide anything: the reconciler re-reads the whole
	// repository, because the payload is a trigger, not a fact.
	Checkout string
}

// eventScopers is dispatch: the events that can leave a repository needing
// adoption or staffing, and how each names its repository.
//
// A map rather than a switch because the set of handled kinds is then a value.
// The manifest's [[events]] subscriptions and this set have to agree in both
// directions — a subscription with no entry here spawns a process per event to
// print "ignoring", an entry here with no subscription is a handler nothing can
// reach — and a switch can only be probed one guess at a time.
var eventScopers = map[EventKind]func(EventData) (Scope, error){
	EventKindWorktreeCreated: func(raw EventData) (Scope, error) {
		var data WorktreeCreatedEvent
		if err := json.Unmarshal(raw, &data); err != nil {
			return Scope{}, err
		}
		return scopeOf(data.Workspace, data.Worktree), nil
	},

	EventKindWorktreeOpened: func(raw EventData) (Scope, error) {
		var data WorktreeOpenedEvent
		if err := json.Unmarshal(raw, &data); err != nil {
			return Scope{}, err
		}
		return scopeOf(data.Workspace, data.Worktree), nil
	},
}

// HandledEventKinds is the set of event kinds Scope resolves, sorted.
func HandledEventKinds() []EventKind {
	kinds := make([]EventKind, 0, len(eventScopers))
	for kind := range eventScopers {
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)
	return kinds
}

// Scope reports which repository an event asks us to reconcile.
//
// Unhandled kinds return ErrUnhandledEvent. The repository comes from the
// payload rather than from the invocation context: the context describes what
// happened to be focused, which is a different question and, for a pane event,
// frequently a different repository.
func (e EventEnvelope) Scope() (Scope, error) {
	scoper, ok := eventScopers[e.Event]
	if !ok {
		return Scope{}, fmt.Errorf("%s: %w", e.Event, ErrUnhandledEvent)
	}

	scope, err := scoper(e.Data)
	if err != nil {
		return Scope{}, fmt.Errorf("decode %s payload: %w", e.Event, err)
	}
	return scope, nil
}

// scopeOf prefers the repository root herdr already resolved, so the common
// path needs no git call at all. It falls back to the checkout, which the
// caller resolves to a root with git.
func scopeOf(workspace WorkspaceInfo, worktree WorktreeInfo) Scope {
	scope := Scope{Checkout: worktree.Path}
	if workspace.Worktree != nil {
		scope.RepoRoot = workspace.Worktree.RepoRoot
	}
	return scope
}
