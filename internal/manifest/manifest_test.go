package manifest

import (
	"context"
	"path/filepath"
	"testing"
)

func TestHappyPathTransitions(t *testing.T) {
	s := Pending
	for {
		next, ok := s.Next()
		if !ok {
			break
		}
		if err := CheckTransition(s, next); err != nil {
			t.Fatal(err)
		}
		s = next
	}
	if s != Done {
		t.Fatalf("ended at %s", s)
	}
}

func TestTransitionRules(t *testing.T) {
	cases := []struct {
		from, to State
		ok       bool
	}{
		{Pending, Transcoded, false},       // cannot skip steps
		{Downloaded, SkippedCorrupt, true}, // outcome from a step
		{Validated, Uploaded, true},        //
		{Validated, Done, true},            // redo produced an identical encode
		{Probed, Done, false},              //
		{Copied, Failed, true},             //
		{Failed, Validated, true},          // retry
		{NeedsConfirm, Validated, true},    // --allow-face-loss
		{Done, Pending, false},             // done is final
		{Done, Probed, true},               // redo
		{Done, Validated, true},            // restore
		{Trashed, Restored, true},          // restore finished
		{Restored, Pending, false},         // reset would point at the trashed replacement
		{Restored, Probed, false},
		{SkippedHDR, Pending, true},     // reset
		{Patched, Validated, true},      // rollback after remote failure
		{Transcoded, Probed, true},      // rewind, work file lost
		{Uploaded, Probed, false},       // no rewind once remote
		{Trashed, Validated, false},     // original already trashed
		{Verified, NeedsConfirm, true},  // faces named mid-run
		{SkippedCorrupt, Pending, true}, // reset after a fixed check
		{Failed, Pending, true},         // reset
		{SkippedCorrupt, Probed, false}, // reset only to pending
	}
	for _, c := range cases {
		err := CheckTransition(c.from, c.to)
		if (err == nil) != c.ok {
			t.Errorf("%s → %s: err=%v, want ok=%v", c.from, c.to, err, c.ok)
		}
	}
}

func TestStatePredicates(t *testing.T) {
	if !Uploaded.Remote() || !Verified.Remote() || Validated.Remote() || Trashed.Remote() {
		t.Error("Remote() wrong")
	}
	if !SkippedLowGain.Permanent() || Failed.Permanent() || NeedsConfirm.Permanent() || !Done.Permanent() || !Restored.Permanent() {
		t.Error("Permanent() wrong")
	}
	if Pending.InProgress() || Done.InProgress() || !Transcoded.InProgress() || Failed.InProgress() {
		t.Error("InProgress() wrong")
	}
}

func TestStore(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	r, err := st.Ensure(ctx, "a1", "clip.mp4", 1000)
	if err != nil || r.State != Pending || r.OriginalSize != 1000 {
		t.Fatalf("ensure: %+v %v", r, err)
	}
	r.BackupPath = "/b/clip.mp4"
	if err := st.Transition(ctx, &r, Downloaded, "ok"); err != nil {
		t.Fatal(err)
	}
	if err := st.Transition(ctx, &r, Done, ""); err == nil {
		t.Fatal("expected invalid transition")
	}
	// Ensure on an existing row keeps its progress.
	again, err := st.Ensure(ctx, "a1", "clip.mp4", 1000)
	if err != nil || again.State != Downloaded || again.BackupPath != "/b/clip.mp4" {
		t.Fatalf("re-ensure: %+v %v", again, err)
	}
	// A stale copy of the record cannot overwrite newer progress.
	stale := again
	if err := st.Transition(ctx, &again, DecodeOK, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.Transition(ctx, &stale, DecodeOK, ""); err == nil {
		t.Fatal("stale transition succeeded")
	}
	states, _ := st.States(ctx)
	if states["a1"] != DecodeOK {
		t.Fatalf("states = %v", states)
	}
}

func TestMigrationAndRedo(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	// A manifest from before source_asset_id existed.
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`ALTER TABLE assets DROP COLUMN source_asset_id`); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if st, err = Open(path); err != nil {
		t.Fatalf("reopen old manifest: %v", err)
	}
	defer st.Close()

	r, _ := st.Ensure(ctx, "orig", "clip.mp4", 100)
	if r.Source() != "orig" {
		t.Errorf("Source() = %q", r.Source())
	}
	if err := st.MarkRedo(ctx, &r); err == nil {
		t.Error("redo of a pending asset allowed")
	}
	for _, to := range []State{Downloaded, DecodeOK, Probed, Transcoded, MetadataCopied, Validated, Uploaded, Copied, Patched, Verified, Trashed, Done} {
		if to == Uploaded {
			r.NewAssetID = "repl1"
		}
		if err := st.Transition(ctx, &r, to, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.MarkRedo(ctx, &r); err != nil {
		t.Fatal(err)
	}
	got, _, _ := st.Get(ctx, "orig")
	if got.State != Probed || got.SourceAssetID != "repl1" || got.NewAssetID != "" || got.Source() != "repl1" {
		t.Errorf("after redo: %+v", got)
	}
	repl, _ := st.Replacements(ctx)
	if repl["repl1"] != "orig" {
		t.Errorf("Replacements = %v", repl)
	}
}
