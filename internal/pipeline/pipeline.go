// Package pipeline runs the per-asset state machine: download, check,
// transcode, validate, upload, copy relationships, verify and trash.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"immich-transcode/internal/config"
	"immich-transcode/internal/immich"
	"immich-transcode/internal/immich/api"
	"immich-transcode/internal/logging"
	"immich-transcode/internal/manifest"
	"immich-transcode/internal/media"
)

// Immich is the subset of the server API the pipeline uses.
type Immich interface {
	GetAsset(ctx context.Context, id string) (*immich.Asset, error)
	Download(ctx context.Context, id string, w ioWriter, progress func(int64)) (string, int64, error)
	Upload(ctx context.Context, u immich.UploadRequest, progress func(int64)) (immich.UploadResult, error)
	CopyRelationships(ctx context.Context, from, to string) error
	UpdateAsset(ctx context.Context, id string, dto api.UpdateAssetDto) error
	TagAsset(ctx context.Context, id string, tagIDs []openapi_types.UUID) error
	Trash(ctx context.Context, ids ...string) error
	Restore(ctx context.Context, id string) error
}

// Options are per-run choices from the command line.
type Options struct {
	Apply         bool // perform remote writes; otherwise stop at Validated
	AllowFaceLoss bool
	RetryFailed   bool
	CRF           int
	// CRFExplicit means CRF came from --crf rather than config: an existing
	// local encode at a different CRF is then redone. Otherwise the existing
	// encode is kept, so a later run cannot silently change its quality.
	CRFExplicit bool
}

// Reencodes reports whether rec has a local encode (not yet uploaded) that
// these options would replace because --crf asks for a different value.
func (o Options) Reencodes(rec manifest.Record) bool {
	return o.CRFExplicit && rec.CRF != o.CRF && hasLocalEncode(rec.State)
}

// HasWork reports whether processing rec with these options would do
// anything: a dry run has nothing left to do for a validated asset unless it
// is to be re-encoded.
func (o Options) HasWork(rec manifest.Record) bool {
	if rec.State == manifest.Validated && !o.Apply {
		return o.Reencodes(rec)
	}
	return true
}

// hasLocalEncode is true from Transcoded up to (not including) Uploaded.
func hasLocalEncode(s manifest.State) bool {
	return atOrAfter(s, manifest.Transcoded) && !atOrAfter(s, manifest.Uploaded)
}

// Processor runs assets through the pipeline.
type Processor struct {
	Cfg     config.Config
	Opts    Options
	Immich  Immich
	Tools   media.Tools
	Store   *manifest.Store
	Log     *slog.Logger
	Console *logging.Console
	UserID  string

	// PollInterval is how often a new asset is polled for metadata.
	PollInterval time.Duration
	// SettleTimeout bounds how long verification waits for a date or
	// location patch to take effect (default 2 minutes).
	SettleTimeout time.Duration

	stopping atomic.Bool
}

// RequestStop asks workers to stop after their current step.
func (p *Processor) RequestStop() { p.stopping.Store(true) }

// Outcome summarises one asset's processing in this run.
type Outcome struct {
	AssetID     string
	FileName    string
	State       manifest.State
	OldSize     int64
	NewSize     int64
	Err         error
	Interrupted bool
}

// Saved is the number of bytes removed from the library by this asset.
func (o Outcome) Saved() int64 {
	if o.State != manifest.Done {
		return 0
	}
	return o.OldSize - o.NewSize
}

// errInterrupted marks a step abandoned because of Ctrl-C.
var errInterrupted = errors.New("interrupted")

// Run processes ids with Cfg.Concurrency workers and returns per-asset
// outcomes in completion order.
func (p *Processor) Run(ctx context.Context, ids []string) []Outcome {
	if p.PollInterval == 0 {
		p.PollInterval = 3 * time.Second
	}
	if p.Cfg.Concurrency > 1 {
		p.Console.DisableStatus()
	}
	work := make(chan string)
	var mu sync.Mutex
	var outcomes []Outcome
	var wg sync.WaitGroup
	for range p.Cfg.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range work {
				o := p.Process(ctx, id)
				mu.Lock()
				outcomes = append(outcomes, o)
				mu.Unlock()
			}
		}()
	}
feed:
	for _, id := range ids {
		if p.stopping.Load() || ctx.Err() != nil {
			break
		}
		select {
		case work <- id:
		case <-ctx.Done():
			break feed
		}
	}
	close(work)
	wg.Wait()
	return outcomes
}

// Process advances one asset as far as the options allow.
func (p *Processor) Process(ctx context.Context, id string) Outcome {
	out := Outcome{AssetID: id}
	// The asset being replaced is the original, or during a redo the
	// previous replacement.
	source := id
	if rec, ok, err := p.Store.Get(ctx, id); err != nil {
		out.Err = err
		return out
	} else if ok {
		source = rec.Source()
	} else if orig, isRepl, err := p.isReplacement(ctx, id); err != nil {
		out.Err = err
		return out
	} else if isRepl {
		// Re-encoding a replacement would compress an already compressed
		// video; redo works from the original's backup instead.
		out.Err = fmt.Errorf("%s is a replacement made by immich-transcode for %s; use `redo --asset %s`", id, orig, orig)
		p.Console.Printf("%v", out.Err)
		return out
	}
	rctx, cancel := remoteCtx(ctx)
	asset, err := p.Immich.GetAsset(rctx, source)
	cancel()
	if err != nil {
		out.Err = err
		p.Console.Printf("%s: %v", id, err)
		return out
	}
	out.FileName = asset.OriginalFileName
	// Checked before the manifest is touched: other users' assets (partner
	// or shared-album videos) are never recorded, downloaded or changed.
	if asset.OwnerId.String() != p.UserID {
		out.Err = errNotOwner
		p.Console.Printf("%s: %v", asset.OriginalFileName, out.Err)
		return out
	}
	rec, err := p.Store.Ensure(ctx, id, asset.OriginalFileName, fileSize(asset))
	if err != nil {
		out.Err = err
		return out
	}
	log := p.Log.With("asset", id, "file", asset.OriginalFileName)

	if err := p.prepare(ctx, &rec, asset); err != nil {
		out.Err = err
		out.State = rec.State
		if !errors.Is(err, errNothingToDo) {
			p.Console.Printf("%s: %v", asset.OriginalFileName, err)
		}
		return p.finish(out, rec)
	}

	for !rec.State.Terminal() {
		if p.stopping.Load() || ctx.Err() != nil {
			out.Interrupted = true
			break
		}
		if rec.State == manifest.Validated && !p.Opts.Apply {
			// Dry run ends here, but still report assets that --apply
			// would hold back for face loss.
			ok, why, err := p.faceGate(ctx, &rec)
			if err == nil && !ok {
				rec.ResumeFrom = manifest.Validated
				err = p.Store.Transition(ctx, &rec, manifest.NeedsConfirm, why)
				p.Console.Printf("%s: %s (%s)", asset.OriginalFileName, manifest.NeedsConfirm, why)
			}
			if err != nil {
				out.Err = err
			}
			break
		}
		from := rec.State
		started := time.Now()
		to, msg, err := p.step(ctx, &rec, asset)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, errInterrupted) {
				log.Warn("step interrupted", "state", from, "err", err)
				out.Interrupted = true
				break
			}
			p.fail(ctx, &rec, err, log)
			out.Err = err
			break
		}
		if err := p.Store.Transition(ctx, &rec, to, msg); err != nil {
			out.Err = fmt.Errorf("record %s: %w", to, err)
			log.Error("manifest write failed", "err", err)
			break
		}
		log.Info("step", "from", from, "to", to, "msg", msg, "took", time.Since(started).Round(time.Millisecond).String())
		if to.Terminal() && to != manifest.Done {
			p.Console.Printf("%s: %s (%s)", asset.OriginalFileName, to, msg)
		}
	}
	p.Console.Status("")
	out.State = rec.State
	return p.finish(out, rec)
}

func (p *Processor) finish(out Outcome, rec manifest.Record) Outcome {
	out.OldSize, out.NewSize = rec.OriginalSize, rec.NewSize
	out.State = rec.State
	switch {
	case out.State == manifest.Restored:
		p.Console.Printf("%s: original restored (%s); replacement %s moved to trash", out.FileName,
			config.HumanBytes(rec.OriginalSize), rec.SourceAssetID)
	case out.State == manifest.Done:
		p.Console.Printf("%s: %s → %s (%.1f%% saved, crf %d) done", out.FileName,
			config.HumanBytes(out.OldSize), config.HumanBytes(out.NewSize), pct(out.OldSize, out.NewSize), rec.CRF)
	case out.State == manifest.Validated && !p.Opts.Apply && !out.Interrupted:
		p.Console.Printf("%s: %s → %s (%.1f%% saved, crf %d) validated; dry run, not uploaded (use --apply)", out.FileName,
			config.HumanBytes(out.OldSize), config.HumanBytes(out.NewSize), pct(out.OldSize, out.NewSize), rec.CRF)
	case out.Interrupted:
		p.Console.Printf("%s: stopped at %s; run `resume` to continue", out.FileName, out.State)
	}
	return out
}

var (
	errNothingToDo = errors.New("nothing to do")
	errNotOwner    = errors.New("asset belongs to another user; only your own videos are processed")
)

// prepare handles retries and reconciles the record with files on disk and
// the asset on the server before any step runs.
func (p *Processor) prepare(ctx context.Context, rec *manifest.Record, asset *immich.Asset) error {
	switch rec.State {
	case manifest.Failed:
		if !p.Opts.RetryFailed {
			return fmt.Errorf("previously failed (%s); use --retry-failed to try again", rec.Error)
		}
	case manifest.NeedsConfirm:
		if !p.Opts.AllowFaceLoss {
			return fmt.Errorf("has named people (%s); use --allow-face-loss to proceed", rec.People)
		}
	case manifest.Done:
		return errNothingToDo
	}
	if rec.State.Permanent() {
		return errNothingToDo
	}
	if rec.State == manifest.Failed || rec.State == manifest.NeedsConfirm {
		from := rec.ResumeFrom
		if from == "" {
			from = manifest.Pending
		}
		rec.Error = ""
		if err := p.Store.Transition(ctx, rec, from, "retry"); err != nil {
			return err
		}
	}
	if asset.IsTrashed && !rec.State.Remote() && rec.State != manifest.Trashed {
		return fmt.Errorf("original is in the trash; not processing")
	}
	return p.reconcileLocal(ctx, rec)
}

// isReplacement reports whether id is an asset this tool uploaded, and the
// original it stands in for.
func (p *Processor) isReplacement(ctx context.Context, id string) (string, bool, error) {
	repl, err := p.Store.Replacements(ctx)
	if err != nil {
		return "", false, err
	}
	orig, ok := repl[id]
	return orig, ok, nil
}

// reconcileLocal moves a not-yet-uploaded record back to an earlier step
// when its files are gone (e.g. the work directory was cleared) or when
// --crf asks for a different encode. The backup is reused, never re-fetched
// unless it is missing.
func (p *Processor) reconcileLocal(ctx context.Context, rec *manifest.Record) error {
	if rec.State.Remote() || rec.State == manifest.Trashed || rec.State == manifest.Pending {
		return nil
	}
	target, why := rec.State, ""
	backupMissing := rec.BackupPath == "" || !exists(rec.BackupPath)
	if backupMissing && rec.SourceAssetID != "" {
		// Downloading "the original" would fetch the previous replacement.
		return fmt.Errorf("cannot redo: the backup of the original (%s) is missing", rec.BackupPath)
	}
	switch {
	case backupMissing:
		target, why = manifest.Pending, "rewind: backup missing"
	case hasLocalEncode(rec.State) && (rec.OutputPath == "" || !exists(rec.OutputPath)):
		target, why = manifest.Probed, "rewind: work file missing"
	case p.Opts.Reencodes(*rec):
		target, why = manifest.Probed, fmt.Sprintf("rewind: re-encode at crf %d (was %d)", p.Opts.CRF, rec.CRF)
		p.Console.Printf("%s: re-encoding at crf %d (existing encode is crf %d)", rec.FileName, p.Opts.CRF, rec.CRF)
	case hasLocalEncode(rec.State) && rec.CRF != p.Opts.CRF:
		p.Console.Printf("%s: keeping existing crf %d encode (pass --crf %d to re-encode)", rec.FileName, rec.CRF, p.Opts.CRF)
	}
	if target == rec.State {
		return nil
	}
	return p.Store.Transition(ctx, rec, target, why)
}

// step runs the action for rec's current state and returns the next state.
func (p *Processor) step(ctx context.Context, rec *manifest.Record, asset *immich.Asset) (manifest.State, string, error) {
	switch rec.State {
	case manifest.Pending:
		return p.download(ctx, rec, asset)
	case manifest.Downloaded:
		return p.decodeCheck(ctx, rec)
	case manifest.DecodeOK:
		return p.probe(ctx, rec)
	case manifest.Probed:
		return p.transcode(ctx, rec, asset)
	case manifest.Transcoded:
		return p.copyMetadata(ctx, rec)
	case manifest.MetadataCopied:
		return p.validate(ctx, rec)
	case manifest.Validated:
		return p.upload(ctx, rec, asset)
	case manifest.Uploaded:
		return p.copyRelationships(ctx, rec)
	case manifest.Copied:
		return p.patch(ctx, rec)
	case manifest.Patched:
		return p.verifyTimeline(ctx, rec)
	case manifest.Verified:
		return p.trashOriginal(ctx, rec)
	case manifest.Trashed:
		return p.complete(rec)
	}
	return "", "", fmt.Errorf("no step for state %s", rec.State)
}

// fail records err. If a new asset exists in Immich it is trashed first so
// the original remains the only copy in the timeline.
func (p *Processor) fail(ctx context.Context, rec *manifest.Record, err error, log *slog.Logger) {
	log.Error("step failed", "state", rec.State, "err", err)
	rec.Error = err.Error()
	rec.ResumeFrom = rec.State
	if rec.State.Remote() && rec.NewAssetID != "" {
		rctx, cancel := remoteCtx(ctx)
		rbErr := p.Immich.Trash(rctx, rec.NewAssetID)
		cancel()
		if rbErr != nil {
			rec.Error += fmt.Sprintf("; ROLLBACK FAILED, new asset %s is still in the library: %v", rec.NewAssetID, rbErr)
			log.Error("rollback failed", "new_asset", rec.NewAssetID, "err", rbErr)
		} else {
			log.Info("rolled back new asset", "new_asset", rec.NewAssetID)
			rec.Error += fmt.Sprintf(" (new asset %s moved to trash; original kept)", rec.NewAssetID)
			rec.NewAssetID = ""
			rec.ResumeFrom = manifest.Validated
		}
	}
	if terr := p.Store.Transition(ctx, rec, manifest.Failed, rec.Error); terr != nil {
		log.Error("manifest write failed", "err", terr)
	}
	p.Console.Printf("%s: FAILED at %s: %s", rec.FileName, rec.ResumeFrom, rec.Error)
}

// remoteCtx gives server calls a context that Ctrl-C does not cancel, so a
// request in flight completes and its result can be recorded.
func remoteCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
}

func fileSize(a *immich.Asset) int64 {
	if a.ExifInfo != nil && a.ExifInfo.FileSizeInByte != nil {
		return int64(*a.ExifInfo.FileSizeInByte)
	}
	return 0
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func atOrAfter(s, ref manifest.State) bool {
	for x := ref; ; {
		if s == x {
			return true
		}
		n, ok := x.Next()
		if !ok {
			return false
		}
		x = n
	}
}

func pct(oldSize, newSize int64) float64 {
	if oldSize <= 0 {
		return 0
	}
	return 100 * float64(oldSize-newSize) / float64(oldSize)
}

func joinNames(names []string) string { return strings.Join(names, ", ") }
