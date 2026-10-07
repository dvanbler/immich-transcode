// Package manifest records each asset's progress through the pipeline in a
// SQLite database so runs are resumable and idempotent.
package manifest

import "fmt"

// State is an asset's position in the pipeline. Each non-terminal state
// names the last step that completed successfully.
type State string

const (
	Pending        State = "pending"
	Downloaded     State = "downloaded"      // backup written and checksum verified
	DecodeOK       State = "decode_ok"       // original decodes cleanly
	Probed         State = "probed"          // ffprobe ran; worth transcoding
	Transcoded     State = "transcoded"      // HEVC output in work dir
	MetadataCopied State = "metadata_copied" // exiftool copied tags into output
	Validated      State = "validated"       // output passed all checks; dry run stops here
	Uploaded       State = "uploaded"        // new asset exists in Immich
	Copied         State = "copied"          // albums, favorite, links, stack, sidecar copied
	Patched        State = "patched"         // remaining field differences fixed
	Verified       State = "verified"        // timeline date and location confirmed
	Trashed        State = "trashed"         // original moved to Immich trash

	Done           State = "done"
	SkippedCorrupt State = "skipped_corrupt"
	SkippedHDR     State = "skipped_hdr"
	SkippedCodec   State = "skipped_codec"
	SkippedEff     State = "skipped_efficient"
	SkippedLowGain State = "skipped_low_gain"
	SkippedOther   State = "skipped_unsupported"
	NeedsConfirm   State = "needs_confirm"
	Failed         State = "failed"
)

// order lists the progress states in pipeline order.
var order = []State{
	Pending, Downloaded, DecodeOK, Probed, Transcoded, MetadataCopied,
	Validated, Uploaded, Copied, Patched, Verified, Trashed, Done,
}

var rank = func() map[State]int {
	m := make(map[State]int, len(order))
	for i, s := range order {
		m[s] = i
	}
	return m
}()

// Terminal states are never processed again by run or resume, except
// NeedsConfirm (with --allow-face-loss) and Failed (with --retry-failed).
func (s State) Terminal() bool {
	switch s {
	case Done, SkippedCorrupt, SkippedHDR, SkippedCodec, SkippedEff,
		SkippedLowGain, SkippedOther, NeedsConfirm, Failed:
		return true
	}
	return false
}

// Permanent states mean the asset should not be selected again.
func (s State) Permanent() bool {
	return s.Terminal() && s != NeedsConfirm && s != Failed
}

// InProgress is true for states between Pending and Done.
func (s State) InProgress() bool {
	_, ok := rank[s]
	return ok && s != Pending && s != Done
}

// Remote is true once Immich holds a new asset for this record, i.e. a
// failure must roll the new asset back.
func (s State) Remote() bool {
	r, ok := rank[s]
	return ok && r >= rank[Uploaded] && r < rank[Trashed]
}

// Next returns the state that follows s in the pipeline.
func (s State) Next() (State, bool) {
	r, ok := rank[s]
	if !ok || r+1 >= len(order) {
		return "", false
	}
	return order[r+1], true
}

// CheckTransition reports whether from → to is allowed: one step forward,
// any non-terminal state to a terminal outcome, or a reset for retry.
func CheckTransition(from, to State) error {
	if next, ok := from.Next(); ok && next == to {
		return nil
	}
	if !from.Terminal() && to.Terminal() && to != Done {
		return nil
	}
	if to == Done && from == Trashed {
		return nil
	}
	// Redo whose fresh encode is byte-identical to the current replacement:
	// nothing is uploaded or trashed, the asset is simply done again.
	if from == Validated && to == Done {
		return nil
	}
	// Redo: a done asset is re-encoded from its backup to replace its
	// current replacement (see the redo command).
	if from == Done && to == Probed {
		return nil
	}
	// Reset: a skipped or failed asset is re-evaluated from the start
	// (e.g. after a check was fixed or thresholds changed).
	if to == Pending && from.Terminal() && from != Done && from != NeedsConfirm {
		return nil
	}
	// Retrying: a failed or needs_confirm asset resumes from a progress
	// state recorded before the failure.
	if (from == Failed || from == NeedsConfirm) && !to.Terminal() {
		return nil
	}
	// Rollback after a remote failure resets to Validated.
	if from.Remote() && to == Validated {
		return nil
	}
	// Rewind: before anything is uploaded, a missing work file sends the
	// asset back to an earlier local step.
	if fr, ok := rank[from]; ok && fr <= rank[Validated] {
		if tr, ok := rank[to]; ok && tr < fr {
			return nil
		}
	}
	return fmt.Errorf("invalid state transition %s → %s", from, to)
}
