package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/steig/worktender/internal/herdrapi"
)

// The metadata channel is how a report actually reaches a gate.
//
// The pane was the original channel and it does not carry a report off a Claude
// Code worker at all: that TUI collapses a finished tool call to "Ran 1 shell
// command", so a worker which RUNS `worktender report` leaves the envelope inside
// its own transcript and nothing on the screen the gate reads. The pair only
// worked when the dispatch prompt talked the worker into reproducing the
// envelope as its reply text — a sentence in a prompt, which is not a mechanism,
// and which fails the first time somebody dispatches a worker without it.
//
// herdr's pane metadata tokens are the channel that does not depend on what a
// TUI chose to render. The worker writes them over the socket it already has,
// herdr stores them against the pane, and the gate reads them back from herdr
// rather than from a terminal.
//
// WHAT TRAVELS HERE, AND WHY ALL OF IT DOES. The gate's predicate surface is
// `status` and `pr` — see gate.go for why the note is deliberately unreachable
// from it — and both fit a token with room to spare, so the control signal was
// never in doubt. The note was: it is 200 runes against a slot that holds 80.
//
// It is chunked across three slots rather than left behind, because the note is
// the whole reason the envelope has a note. A coordinator that releases on a
// `done` and gets no summary with it has to go and read the worker's pane to
// find out what happened, which is the channel this exists to stop depending on.
// A note capped at 80 for this channel would have been the same loss wearing a
// smaller number, and `worktender report` would then accept a 200-rune note and
// deliver 80 of it.
//
// Chunking is safe here for one reason and it is not the arithmetic: the joined
// note runs back through reportNote before it is a report at all. Whatever
// arrives in those slots — a stale chunk, a chunk some other writer put there,
// a chunk with a newline in it — either reassembles into something reportNote
// already accepts, or it is not a report. Reassembly cannot widen what the
// writer's validation allows, because it is the writer's validation.
//
// WHAT IDENTIFIES ONE REPORT. A counter, not the content. The slots are a fixed
// template and a coordinator that dispatches the same kind of slice twice gets
// two byte-identical reports; a reader that told them apart by comparing them
// would hear the second one as a repeat of the first and wait out its timeout on
// a worker that had answered. So every write carries worktender_seq, one higher than
// the one it found on the pane, and a report is a DIFFERENT report exactly when
// the counter moved. It is not a predicate surface and the gate cannot match on
// it — it decides only whether there is something new to judge.
//
// WHAT DOES NOT TRAVEL HERE. There is no free-text slot, no second note, and
// nothing the gate can form a predicate over that it could not before. The
// channel changed; the envelope did not.

const (
	// tokenSource is the provenance herdr files these writes under. It is NOT a
	// namespace — herdr keeps one token map per pane and every writer merges
	// into it — so the keys below carry the namespace themselves.
	tokenSource = "steig.worktender"

	// tokenValueLimit is how much of a token value herdr keeps, in runes, which
	// is the same unit noteLimit counts in.
	//
	// Measured on herdr 0.7.5, in both directions: 80 runes in stores 80, and 81
	// in stores 80. There is no error, no warning, and no mention of the limit
	// in the schema. Everything in this file that looks like excessive care
	// about lengths is care about that.
	tokenValueLimit = 80
)

// The keys, all under one prefix because the map is shared with every other
// writer on the pane, and versioned because the presence of tokenKeyVersion is
// what distinguishes "worktender wrote a report here" from "some keys exist".
const (
	tokenKeyVersion = "worktender_v"
	tokenKeyStatus  = "worktender_status"
	tokenKeyPR      = "worktender_pr"
	// The note's chunks are this plus their index: worktender_note0, worktender_note1…
	tokenKeyNotePrefix = "worktender_note"
	// worktender_seq counts reports on this pane. See WHAT IDENTIFIES ONE REPORT
	// above; it says which report this is, never what it says.
	tokenKeySeq = "worktender_seq"
)

// tokenVersion is the layout above. It moves when a reader of the old layout
// would misread the new one, and a reader that does not recognise it treats the
// pane as carrying no report rather than guessing.
//
// worktender_seq arriving did not move it, on that rule: a reader without it reads
// every slot of a report carrying one exactly as its author wrote them. What it
// cannot do is tell two of them apart, which is the bug the counter exists to
// fix and not a misreading of anything.
const tokenVersion = "1"

// noteChunks is how many slots a full-length note needs. Derived from the two
// limits so the layout cannot drift from either of them.
const noteChunks = (noteLimit + tokenValueLimit - 1) / tokenValueLimit

func noteChunkKey(i int) string { return fmt.Sprintf("%s%d", tokenKeyNotePrefix, i) }

// encodeReport lays a report out across tokens.
//
// Every slot this plugin owns is written on EVERY report, and the note chunks a
// short note does not need are written as explicit nulls. That is not tidiness.
// A write merges, so a chunk left unnamed is a chunk left over from the previous
// report, and the next reader would join a sentence that neither report
// contained and that both would disown.
func encodeReport(r report, seq uint64) (map[string]any, error) {
	// Zero is the value a reader uses for "this pane carries no report", so a
	// report claiming it could never be read back. The only way to get here is a
	// counter that wrapped, and a loud refusal beats delivering an envelope no
	// gate can see.
	if seq == 0 {
		return nil, fmt.Errorf("a report cannot be numbered 0; %s counts from 1", tokenKeySeq)
	}

	tokens := map[string]any{
		tokenKeyVersion: tokenVersion,
		tokenKeyStatus:  r.status,
		tokenKeyPR:      prSlot(r),
		tokenKeySeq:     strconv.FormatUint(seq, 10),
	}

	chunks := chunkRunes(r.note, tokenValueLimit)
	if len(chunks) > noteChunks {
		return nil, fmt.Errorf("the note needs %d token slots and the layout has %d; it cannot be delivered without losing the end of it", len(chunks), noteChunks)
	}
	for i := range noteChunks {
		if i < len(chunks) {
			tokens[noteChunkKey(i)] = chunks[i]
			continue
		}
		tokens[noteChunkKey(i)] = nil
	}

	// The belt to writeReport's braces, and the one that names the slot rather
	// than the symptom. herdr would cut an over-long value to 80 runes and say
	// nothing, so a slot that grew past the limit would surface as a report that
	// mysteriously fails to confirm.
	for key, value := range tokens {
		text, ok := value.(string)
		if !ok {
			continue
		}
		if n := utf8.RuneCountInString(text); n > tokenValueLimit {
			return nil, fmt.Errorf("token %s is %d characters and herdr stores %d, cutting the rest without saying so", key, n, tokenValueLimit)
		}
	}
	return tokens, nil
}

// decodeReport reads a report back out of a pane's tokens, all-or-nothing,
// along with the sequence that identifies it. The sequence is zero exactly when
// there is no report to return.
//
// It is the metadata twin of parseEnvelope and inherits the same guarantee the
// same way: every slot goes back through the validator the writer used, so a
// token map assembled by something that is not `worktender report` is rejected
// rather than half-read.
func decodeReport(tokens map[string]string) (report, uint64, bool) {
	if tokens[tokenKeyVersion] != tokenVersion {
		return report{}, 0, false
	}

	seq, ok := parseSeqValue(tokens[tokenKeySeq])
	if !ok {
		return report{}, 0, false
	}

	status := tokens[tokenKeyStatus]
	if !isReportStatus(status) {
		return report{}, 0, false
	}

	pr, ok := parsePRValue(tokens[tokenKeyPR])
	if !ok {
		return report{}, 0, false
	}

	note, ok := joinNoteChunks(tokens)
	if !ok {
		return report{}, 0, false
	}
	// The joined note, not the chunks. A chunk is a fragment and means nothing
	// on its own; what a coordinator will be shown is the join, so the join is
	// what has to survive the validation the note slot has always been under.
	if reportNote(note) != nil {
		return report{}, 0, false
	}
	return report{status: status, pr: pr, note: note}, seq, true
}

// parseSeqValue reads the counter slot. Absent, zero, or anything that is not a
// plain decimal is not a sequence — and a report without one is not a report,
// because a reader that cannot say which report it is holding cannot say whether
// it has already judged it.
func parseSeqValue(raw string) (uint64, bool) {
	seq, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || seq == 0 {
		return 0, false
	}
	return seq, true
}

// nextSeq is the number the report about to be written gets: one past whatever
// the pane already carries.
//
// A map with no readable counter starts at one, which covers a fresh pane and
// one whose counter something else has since written over. A restart can never
// make a gate release EARLY — a number below the mark is not news, which is the
// direction that matters — but it can cost the restarting report: a gate still
// holding a higher mark reads it as old and hears only the one after it. Getting
// there takes another writer inside this pane's token map, which is the same
// access that can forge a whole report; see readChannels in gate.go.
func nextSeq(tokens map[string]string) uint64 {
	seq, ok := parseSeqValue(tokens[tokenKeySeq])
	if !ok {
		return 1
	}
	return seq + 1
}

// joinNoteChunks reassembles the note from contiguous slots.
//
// A gap is fatal rather than skipped. encodeReport writes the chunks it uses and
// nulls the ones it does not, so slots 0..n-1 present with n+1 also present is a
// shape this plugin cannot produce — it means something else has been writing
// into the map, and joining across the hole would silently splice two different
// reports into one sentence.
func joinNoteChunks(tokens map[string]string) (string, bool) {
	var note strings.Builder
	for i := range noteChunks {
		chunk, present := tokens[noteChunkKey(i)]
		if present {
			note.WriteString(chunk)
			continue
		}
		for j := i + 1; j < noteChunks; j++ {
			if _, later := tokens[noteChunkKey(j)]; later {
				return "", false
			}
		}
		break
	}
	return note.String(), true
}

// chunkRunes splits by runes rather than bytes, because the limit it is cutting
// to is counted in runes and a byte-wise split would also cut a character in
// half.
func chunkRunes(s string, size int) []string {
	var chunks []string
	runes := []rune(s)
	for start := 0; start < len(runes); start += size {
		chunks = append(chunks, string(runes[start:min(start+size, len(runes))]))
	}
	return chunks
}

// deliverReport writes a report to the pane's metadata and proves it landed.
//
// The second return names what was missing when there was nowhere to deliver
// to. Running `worktender report` outside herdr — from a plain shell, to see the
// envelope — is a legitimate thing to do and not a failure, so delivery is
// skipped rather than refused. It is not silent either: the caller says which
// variable was absent, because a worker that cannot tell "delivered" from
// "printed" cannot tell whether it still has to echo.
func deliverReport(r report) (string, error) {
	for _, name := range []string{paneEnv, socketEnv} {
		if os.Getenv(name) == "" {
			return name, nil
		}
	}

	client, err := herdrapi.New()
	if err != nil {
		return "", err
	}
	return "", writeReport(client, os.Getenv(paneEnv), r)
}

const (
	// paneEnv is the pane herdr injects into the shell it starts in one. A
	// worker runs `worktender report` in its own pane, so this is the pane the
	// report is about — a report is never written to a pane its author does not
	// occupy, which would hand a worker the ability to file under another
	// worker's name.
	paneEnv = "HERDR_PANE_ID"

	socketEnv = "HERDR_SOCKET_PATH"
)

// writeReport numbers the report, delivers the tokens, and reads them back.
//
// The number comes from the pane rather than from this process, because the
// process is new every time: a worker runs `worktender report` once per report and
// has no memory of the last one. The pane is where the previous report is, so
// the pane is where the count is kept. Two reports racing for the same number
// would need one worker running `worktender report` twice at once in its own pane,
// which is not a thing a worker does — and herdr's own `seq` param cannot help
// with it anyway, because it is write-only: nothing in pane.get returns it, so a
// reader could not tell the guard had fired.
//
// The read-back is the point of this function. herdr cuts an over-long value to
// 80 runes, strips control characters out of one, and drops an empty one
// entirely — and returns ok for all three, so a writer that trusted the reply
// would report a delivered envelope while the coordinator read a mangled one.
// That is the exact failure this plugin refuses everywhere else, and it is
// worth a round trip to refuse it here.
//
// Comparing what came back also covers the modes nobody has measured yet: the
// claim is not "these three mangling rules are handled" but "what herdr stored
// is what this report said", which stays true whatever herdr adds next.
func writeReport(client *herdrapi.Client, paneID string, r report) error {
	before, err := client.PaneGet(paneID)
	if err != nil {
		return fmt.Errorf("read pane %s before reporting to it: %w", paneID, err)
	}

	tokens, err := encodeReport(r, nextSeq(before.Pane.Tokens))
	if err != nil {
		return err
	}
	if err := client.PaneReportMetadata(paneID, tokenSource, tokens); err != nil {
		return fmt.Errorf("deliver report to pane %s: %w", paneID, err)
	}

	info, err := client.PaneGet(paneID)
	if err != nil {
		return fmt.Errorf("confirm report on pane %s: %w", paneID, err)
	}
	return confirmTokens(paneID, tokens, info.Pane.Tokens)
}

// confirmTokens fails on the first slot herdr stored differently from what it
// was given, naming both so the failure says what was lost rather than that
// something was.
func confirmTokens(paneID string, want map[string]any, stored map[string]string) error {
	for key, value := range want {
		got, present := stored[key]
		text, wanted := value.(string)

		switch {
		case !wanted && present:
			return fmt.Errorf("report not delivered intact to pane %s: herdr kept %s as %q after it was cleared", paneID, key, got)
		case !wanted:
			continue
		case !present:
			return fmt.Errorf("report not delivered intact to pane %s: herdr stored no %s at all", paneID, key)
		case got != text:
			return fmt.Errorf("report not delivered intact to pane %s: herdr stored %s as %q, not the %q this report wrote", paneID, key, got, text)
		}
	}
	return nil
}
