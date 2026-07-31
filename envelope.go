package main

import (
	"strconv"
	"strings"
	"unicode"
)

// Reading an envelope back out of a terminal is the other half of report.go's
// format, and it inherits that file's guarantees by construction: it accepts
// EXACTLY what renderReport writes and nothing else, and it re-runs every slot
// through the same validators parseReport uses.
//
// That is what closes the forged-frame hole at this end. reportNote refuses a
// note containing a newline, so a note can never open a line of its own; a
// worker cannot hide a second `status:` line inside the one slot a hostile
// author can reach. Here the same rule is enforced in reverse — a candidate
// whose note fails reportNote is not an envelope at all, so a frame assembled
// out of text that never went through `worktender report` is rejected rather than
// half-read.
//
// What it does NOT do is authenticate the author. The pane is the worker's own
// output channel and every byte in it is the worker's speech; a worker that
// prints seven crafted lines gets an envelope that parses. See gate.go for what
// that means and why a shared secret would not fix it.

// envelopeFrame is the fixed shape renderReport emits, derived from the same
// constants so the reader cannot drift from the writer.
var envelopeFrame = func() (announce []string, lines int) {
	announce = strings.Split(noteOpen, "\n")
	// header, status, pr, the announcement, the quoted note, the terminator.
	return announce, 3 + len(announce) + 2
}

// envelopesIn returns the last complete envelope in a terminal snapshot, and
// how many the snapshot holds.
//
// Last, not first: a pane accumulates, so an earlier envelope is an earlier
// report and the later one is the answer that stands.
//
// The COUNT is what makes this channel usable to a gate that has to tell a NEW
// report from the one it already judged. An envelope's identity here is its
// POSITION in the output — two byte-identical envelopes are two reports, and the
// second one is news. Comparing the text instead would make a worker that
// reports the same thing twice inaudible the second time. See gate.go for the
// rule this feeds.
//
// A match consumes its whole frame rather than one line. Envelopes cannot
// overlap — every line inside one is a fixed line, a validated slot, or quoted
// behind "> ", and none of those can pass isHeaderLine — so counting frames and
// counting starting positions give the same answer, and stepping by the frame
// says plainly that one envelope is one report.
func envelopesIn(text string) (report, uint64) {
	lines := splitTerminalLines(text)
	_, height := envelopeFrame()

	var last report
	var count uint64
	for start := 0; start+height <= len(lines); {
		r, ok := parseEnvelope(lines[start : start+height])
		if !ok {
			start++
			continue
		}
		last, count = r, count+1
		start += height
	}
	return last, count
}

// parseEnvelope reads one candidate window of lines, all-or-nothing.
func parseEnvelope(lines []string) (report, bool) {
	announce, height := envelopeFrame()
	if len(lines) != height {
		return report{}, false
	}
	if !isHeaderLine(lines[0]) || lines[height-1] != noteClose {
		return report{}, false
	}
	for i, want := range announce {
		if lines[3+i] != want {
			return report{}, false
		}
	}

	status, ok := strings.CutPrefix(lines[1], "status: ")
	if !ok || !isReportStatus(status) {
		return report{}, false
	}

	pr, ok := parsePRSlot(lines[2])
	if !ok {
		return report{}, false
	}

	note, ok := strings.CutPrefix(lines[height-2], noteQuote)
	if !ok || reportNote(note) != nil {
		return report{}, false
	}

	return report{status: status, pr: pr, note: note}, true
}

// isHeaderLine finds the header under the decoration an agent TUI stamps on the
// first line of a message.
//
// This is not generosity, it is the difference between working and not. Claude
// Code renders the first line of an assistant message as "⏺ worktender-report v1",
// so a parser demanding the bare header reads nothing at all from the pane of
// the agent this exists to gate — which is how it was found.
//
// The allowance is bounded to decoration: whatever precedes the header may hold
// no letter and no digit. "⏺ " and a shell prompt's "❯ " qualify; "our own
// worktender-report v1" does not, so a sentence mentioning the header does not open
// a frame.
func isHeaderLine(line string) bool {
	prefix, ok := strings.CutSuffix(line, reportHeader)
	if !ok {
		return false
	}
	return !strings.ContainsFunc(prefix, func(r rune) bool {
		return unicode.IsLetter(r) || unicode.IsDigit(r)
	})
}

// parsePRSlot reads the pr line.
func parsePRSlot(line string) (int, bool) {
	raw, ok := strings.CutPrefix(line, "pr: ")
	if !ok {
		return 0, false
	}
	return parsePRValue(raw)
}

// parsePRValue reads the slot's value, wherever it arrived from, where the dash
// means the worker gave none. Both channels read it through here so a pr that a
// terminal would refuse cannot be smuggled in as a metadata token instead.
func parsePRValue(raw string) (int, bool) {
	if raw == missing {
		return 0, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// splitTerminalLines normalises a snapshot into lines that can be compared for
// equality.
//
// Surrounding whitespace goes because a terminal supplies its own: cells are
// padded out to the pane width on the right, and an agent TUI indents the
// output of the commands it runs on the left. A parser that insisted on column
// zero could not read the pane of the very agent this exists to gate.
//
// It costs nothing the frame was relying on. The frame's claim is that a note
// cannot begin a line, and that is enforced where it has to be — at write time,
// by reportNote refusing newlines — not by which column the line starts in.
func splitTerminalLines(text string) []string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(line)
	}
	return lines
}
