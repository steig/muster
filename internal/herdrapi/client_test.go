package herdrapi

import (
	"testing"
	"time"
)

// A request that asks herdr to wait must not be aborted by a client deadline
// shorter than the wait it requested.
func TestDeadlineOutlastsTheServerSideWait(t *testing.T) {
	for _, tc := range []struct {
		name       string
		serverWait int
		want       time.Duration
	}{
		{"no server-side wait", 0, defaultCallTimeout},
		{"negative is treated as none", -1, defaultCallTimeout},
		{"agent start waits 60s", 60_000, 60*time.Second + callMargin},
		{"short wait still gets the margin", 4_000, 4*time.Second + callMargin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := deadlineFor(tc.serverWait); got != tc.want {
				t.Errorf("deadlineFor(%d) = %v, want %v", tc.serverWait, got, tc.want)
			}
		})
	}
}

// The regression in one line: whatever wait we ask herdr to perform, our own
// deadline has to be longer, or the slow case can never succeed.
func TestDeadlineAlwaysExceedsTheRequestedWait(t *testing.T) {
	for _, waitMS := range []int{1, 1_000, 60_000, 300_000} {
		requested := time.Duration(waitMS) * time.Millisecond
		if got := deadlineFor(waitMS); got <= requested {
			t.Errorf("deadlineFor(%d) = %v, which does not outlast the %v wait it asked for",
				waitMS, got, requested)
		}
	}
}
