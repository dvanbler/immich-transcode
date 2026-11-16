package manifest

import (
	"context"
	"path/filepath"
	"slices"
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
	if !Pending.Valid() || !Verified.Valid() || !SkippedHDR.Valid() || !Failed.Valid() || State("").Valid() || State("bogus").Valid() {
		t.Error("Valid() wrong")
	}
	if !Uploaded.AtLeast(Transcoded) || !Transcoded.AtLeast(Transcoded) || Probed.AtLeast(Transcoded) ||
		Failed.AtLeast(Pending) || Done.AtLeast(Failed) {
		t.Error("AtLeast() wrong")
	}
	if Pending.InProgress() || Done.InProgress() || !Transcoded.InProgress() || Failed.InProgress() {
		t.Error("InProgress() wrong")
	}
}

// All, the descriptions, Terminal and the pipeline order must agree, so help
// text cannot drift from the states the pipeline uses.
func TestAllStatesDescribed(t *testing.T) {
	all := All()
	if len(all) != len(descriptions) {
		t.Errorf("All() has %d states, descriptions has %d", len(all), len(descriptions))
	}
	seen := map[State]bool{}
	for _, s := range all {
		if seen[s] {
			t.Errorf("%s listed twice", s)
		}
		seen[s] = true
		if s.Description() == "" || !s.Valid() {
			t.Errorf("%s has no description", s)
		}
		if _, progress := rank[s]; !progress && !s.Terminal() {
			t.Errorf("%s is neither a pipeline step nor terminal", s)
		}
	}
	for _, s := range outcomes {
		if !s.Terminal() {
			t.Errorf("outcome %s is not terminal", s)
		}
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

func schemaVersion(t *testing.T, st *Store) int {
	t.Helper()
	var v int
	if err := st.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if v := schemaVersion(t, st); v != len(migrations) {
		t.Errorf("new manifest is at schema version %d, want %d", v, len(migrations))
	}
	// A manifest from a newer build is refused rather than misread.
	if _, err := st.db.Exec(`PRAGMA user_version = 9999`); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if st, err := Open(path); err == nil {
		st.Close()
		t.Error("opened a manifest with a newer schema version")
	}
}

// Timestamps written by earlier builds dropped trailing zeros, which does
// not sort as text; the upgrade rewrites them at a fixed width.
func TestTimestampsSortAsText(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "m.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for id, at := range map[string]string{
		"whole-second": "2026-01-01T00:00:00Z",
		"half-later":   "2026-01-01T00:00:00.5Z",
		"unparseable":  "not a time",
	} {
		if _, err := st.db.Exec(`INSERT INTO assets (asset_id, state, created_at, updated_at) VALUES (?, ?, ?, ?)`,
			id, Pending, at, at); err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.Exec(`INSERT INTO events (asset_id, at, from_state, to_state) VALUES (?, ?, '', ?)`, id, at, Pending); err != nil {
			t.Fatal(err)
		}
	}
	// Back to the version before timestamps were fixed.
	if _, err := st.db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if st, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	stored := func(query, id string) string {
		t.Helper()
		var s string
		if err := st.db.QueryRow(query, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	for id, want := range map[string]string{
		"whole-second": "2026-01-01T00:00:00.000000000Z",
		"half-later":   "2026-01-01T00:00:00.500000000Z",
		"unparseable":  "not a time",
	} {
		for _, q := range []string{
			`SELECT updated_at FROM assets WHERE asset_id = ?`,
			`SELECT created_at FROM assets WHERE asset_id = ?`,
			`SELECT at FROM events WHERE asset_id = ?`,
		} {
			if got := stored(q, id); got != want {
				t.Errorf("%s (%s): %q, want %q", id, q, got, want)
			}
		}
	}

	// A new row is written at the same width, and List's "most recently
	// updated first" now holds within a second.
	if _, err := st.Ensure(ctx, "new", "clip.mp4", 1); err != nil {
		t.Fatal(err)
	}
	if got := stored(`SELECT updated_at FROM assets WHERE asset_id = ?`, "new"); len(got) != len(timeLayout)-len("07:00") {
		t.Errorf("new timestamp %q is not fixed width", got)
	}
	if _, err := st.db.Exec(`DELETE FROM assets WHERE asset_id = 'unparseable'`); err != nil {
		t.Fatal(err)
	}
	recs, err := st.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, r := range recs {
		order = append(order, r.AssetID)
	}
	if want := []string{"new", "half-later", "whole-second"}; !slices.Equal(order, want) {
		t.Errorf("List order %v, want %v", order, want)
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
	// Such manifests carry no schema version.
	if _, err := st.db.Exec(`PRAGMA user_version = 0`); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if st, err = Open(path); err != nil {
		t.Fatalf("reopen old manifest: %v", err)
	}
	defer st.Close()
	if v := schemaVersion(t, st); v != len(migrations) {
		t.Errorf("upgraded manifest is at schema version %d, want %d", v, len(migrations))
	}

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
