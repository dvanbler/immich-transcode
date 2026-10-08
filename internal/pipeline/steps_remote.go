package pipeline

import (
	"context"
	"fmt"
	"math"
	"os"
	"sync/atomic"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"immich-transcode/internal/config"
	"immich-transcode/internal/immich"
	"immich-transcode/internal/immich/api"
	"immich-transcode/internal/manifest"
	"immich-transcode/internal/media"
)

// NamedPeople returns the names of recognised people on an asset. Face
// assignments are not carried over to the new asset.
func NamedPeople(a *immich.Asset) []string {
	if a.People == nil {
		return nil
	}
	var names []string
	for _, person := range *a.People {
		if person.Name != "" {
			names = append(names, person.Name)
		}
	}
	return names
}

// faceGate stops before any server write if the original has named people
// and --allow-face-loss was not given.
func (p *Processor) faceGate(ctx context.Context, rec *manifest.Record) (bool, string, error) {
	rctx, cancel := remoteCtx(ctx)
	defer cancel()
	a, err := p.Immich.GetAsset(rctx, rec.Source())
	if err != nil {
		return false, "", err
	}
	names := NamedPeople(a)
	rec.People = joinNames(names)
	if len(names) == 0 || p.Opts.AllowFaceLoss {
		return true, "", nil
	}
	p.Log.Warn("named people would lose their face assignments", "asset", rec.Source(), "people", names)
	return false, fmt.Sprintf("named people would be lost: %s (rerun with --allow-face-loss, then reassign them)", rec.People), nil
}

func (p *Processor) upload(ctx context.Context, rec *manifest.Record, asset *immich.Asset) (manifest.State, string, error) {
	if rec.SourceAssetID != "" && rec.Mode != manifest.ModeRestore {
		// Redo: if the fresh encode is byte-identical to the current
		// replacement (same ffmpeg and settings), there is nothing to
		// replace. Keep the current one.
		_, b64, err := immich.FileSHA1(rec.OutputPath)
		if err != nil {
			return "", "", err
		}
		if b64 == asset.Checksum {
			kept := rec.SourceAssetID
			rec.NewAssetID, rec.SourceAssetID = kept, ""
			os.Remove(rec.OutputPath)
			return manifest.Done, "redo: new encode is identical to the current replacement " + kept + "; kept it", nil
		}
	}
	ok, why, err := p.faceGate(ctx, rec)
	if err != nil {
		return "", "", err
	}
	if !ok {
		rec.ResumeFrom = manifest.Validated
		return manifest.NeedsConfirm, why, nil
	}
	hexSum, _, err := immich.FileSHA1(rec.OutputPath)
	if err != nil {
		return "", "", err
	}
	name := media.OutputName(asset.OriginalFileName, media.ContainerFor(asset.OriginalFileName))
	if rec.Mode == manifest.ModeRestore {
		name = rec.FileName // the backup is the original file, under its own name
	}
	var sent atomic.Int64
	stop := p.ticker(func() string {
		return fmt.Sprintf("%s: uploading %s", rec.FileName, progressText(sent.Load(), rec.NewSize))
	})
	res, err := p.Immich.Upload(ctx, immich.UploadRequest{
		Path:           rec.OutputPath,
		FileName:       name,
		FileCreatedAt:  asset.FileCreatedAt,
		FileModifiedAt: asset.FileModifiedAt,
		SHA1Hex:        hexSum,
		Visibility:     asset.Visibility,
		IsFavorite:     asset.IsFavorite,
	}, func(n int64) { sent.Add(n) })
	stop()
	if err != nil {
		if ctx.Err() != nil {
			return "", "", errInterrupted
		}
		return "", "", err
	}
	msg := "uploaded as " + res.ID
	if res.Duplicate {
		// The same bytes were uploaded before: a run interrupted after the
		// upload, or a retry after rollback trashed the earlier copy.
		if err := p.adoptDuplicate(ctx, rec, res.ID); err != nil {
			return "", "", err
		}
		msg = "server already had this file as " + res.ID
	}
	rec.NewAssetID = res.ID
	return manifest.Uploaded, msg, nil
}

func (p *Processor) adoptDuplicate(ctx context.Context, rec *manifest.Record, id string) error {
	restoringOriginal := rec.Mode == manifest.ModeRestore && id == rec.AssetID
	if id == rec.Source() || (id == rec.AssetID && !restoringOriginal) {
		return fmt.Errorf("server reports the upload as a duplicate of the asset it replaces")
	}
	// A restore whose original is still in the trash lands here: the backup
	// has the original's checksum. Restoring it brings back the original
	// asset, including its face assignments.
	rctx, cancel := remoteCtx(ctx)
	defer cancel()
	dup, err := p.Immich.GetAsset(rctx, id)
	if err != nil {
		return err
	}
	if dup.OwnerId.String() != p.UserID {
		return fmt.Errorf("duplicate asset %s belongs to another user", id)
	}
	if dup.IsTrashed {
		if err := p.Immich.Restore(rctx, id); err != nil {
			return fmt.Errorf("restore duplicate %s from trash: %w", id, err)
		}
	}
	return nil
}

func (p *Processor) copyRelationships(ctx context.Context, rec *manifest.Record) (manifest.State, string, error) {
	p.Console.Status(rec.FileName + ": copying albums, favorite, shared links, stack, sidecar")
	rctx, cancel := remoteCtx(ctx)
	defer cancel()
	if err := p.Immich.CopyRelationships(rctx, rec.Source(), rec.NewAssetID); err != nil {
		return "", "", err
	}
	return manifest.Copied, "albums, favorite, shared links, stack, sidecar", nil
}

// waitForMetadata polls the new asset until Immich has extracted its
// metadata, so patches are not overwritten by the extraction job.
//
// hasMetadata alone is not enough: on v3.2.4 it is already true right after
// upload, while localDateTime still holds the upload's fileCreatedAt and the
// time zone is unknown. Duration and dimensions are written by the same
// extraction pass that sets the time zone, so they mark completion.
func (p *Processor) waitForMetadata(ctx context.Context, rec *manifest.Record) (*immich.Asset, error) {
	deadline := time.Now().Add(p.Cfg.MetadataTimeout)
	for {
		rctx, cancel := remoteCtx(ctx)
		a, err := p.Immich.GetAsset(rctx, rec.NewAssetID)
		cancel()
		if err != nil {
			return nil, err
		}
		if ExtractionDone(a) {
			return a, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("metadata extraction for %s not finished after %s", rec.NewAssetID, p.Cfg.MetadataTimeout)
		}
		p.Console.Status(rec.FileName + ": waiting for Immich metadata extraction")
		select {
		case <-ctx.Done():
			return nil, errInterrupted
		case <-time.After(p.PollInterval):
		}
	}
}

// ExtractionDone reports whether Immich has finished extracting metadata
// for a video asset.
func ExtractionDone(a *immich.Asset) bool {
	return a.HasMetadata && a.ExifInfo != nil && a.Duration != nil && a.Width != nil && a.Height != nil
}

func (p *Processor) patch(ctx context.Context, rec *manifest.Record) (manifest.State, string, error) {
	newA, err := p.waitForMetadata(ctx, rec)
	if err != nil {
		return "", "", err
	}
	rctx, cancel := remoteCtx(ctx)
	defer cancel()
	oldA, err := p.Immich.GetAsset(rctx, rec.Source())
	if err != nil {
		return "", "", err
	}
	dto, fields := Diff(oldA, newA)
	if len(fields) > 0 {
		if err := p.Immich.UpdateAsset(rctx, rec.NewAssetID, dto); err != nil {
			return "", "", err
		}
	}
	if missing := MissingTags(oldA, newA); len(missing) > 0 {
		if err := p.Immich.TagAsset(rctx, rec.NewAssetID, missing); err != nil {
			return "", "", err
		}
		fields = append(fields, fmt.Sprintf("tags(%d)", len(missing)))
	}
	if len(fields) == 0 {
		return manifest.Patched, "no differences", nil
	}
	return manifest.Patched, fmt.Sprintf("patched %v", fields), nil
}

// Diff builds an update that makes newA match oldA on the fields the copy
// endpoint does not handle. fields names what changed.
//
// Favorite and visibility are normally already right (sent with the upload);
// they are still compared because a duplicate upload ignores those fields.
// Description, rating, date and location can only be written through the
// deprecated PUT /assets/{id}.
func Diff(oldA, newA *immich.Asset) (api.UpdateAssetDto, []string) {
	var dto api.UpdateAssetDto
	var fields []string
	oe, ne := exif(oldA), exif(newA)

	if d := strOr(oe.Description); d != strOr(ne.Description) {
		dto.Description = &d
		fields = append(fields, "description")
	}
	if oe.Rating != nil && (ne.Rating == nil || *ne.Rating != *oe.Rating) {
		dto.Rating = oe.Rating
		fields = append(fields, "rating")
	}
	if oldA.IsFavorite != newA.IsFavorite {
		fav := oldA.IsFavorite
		dto.IsFavorite = &fav
		fields = append(fields, "favorite")
	}
	if oldA.Visibility != newA.Visibility {
		v := oldA.Visibility
		dto.Visibility = &v
		fields = append(fields, "visibility")
	}
	if oe.DateTimeOriginal != nil && (ne.DateTimeOriginal == nil || !oe.DateTimeOriginal.Equal(*ne.DateTimeOriginal)) {
		s := FormatDateTimeOriginal(*oe.DateTimeOriginal, oldA.LocalDateTime)
		dto.DateTimeOriginal = &s
		fields = append(fields, "dateTimeOriginal")
	}
	if oe.Latitude != nil && oe.Longitude != nil &&
		(!sameCoord(oe.Latitude, ne.Latitude) || !sameCoord(oe.Longitude, ne.Longitude)) {
		dto.Latitude, dto.Longitude = oe.Latitude, oe.Longitude
		fields = append(fields, "location")
	}
	return dto, fields
}

// FormatDateTimeOriginal renders an instant with the UTC offset implied by
// Immich's localDateTime (local wall time stored as if it were UTC), so the
// server derives the same local date.
func FormatDateTimeOriginal(instant, localDateTime time.Time) string {
	offset := localDateTime.Sub(instant).Round(time.Minute)
	if offset < -14*time.Hour || offset > 14*time.Hour {
		offset = 0
	}
	zone := time.FixedZone("", int(offset.Seconds()))
	return instant.In(zone).Format("2006-01-02T15:04:05.000-07:00")
}

// MissingTags lists tag ids on oldA that newA lacks.
func MissingTags(oldA, newA *immich.Asset) []openapi_types.UUID {
	if oldA.Tags == nil {
		return nil
	}
	have := map[openapi_types.UUID]bool{}
	if newA.Tags != nil {
		for _, t := range *newA.Tags {
			have[t.Id] = true
		}
	}
	var missing []openapi_types.UUID
	for _, t := range *oldA.Tags {
		if !have[t.Id] {
			missing = append(missing, t.Id)
		}
	}
	return missing
}

// verifyTimeline confirms the new asset sits at the same place in the
// timeline and on the map as the original.
func (p *Processor) verifyTimeline(ctx context.Context, rec *manifest.Record) (manifest.State, string, error) {
	newA, err := p.waitForMetadata(ctx, rec)
	if err != nil {
		return "", "", err
	}
	rctx, cancel := remoteCtx(ctx)
	oldA, err := p.Immich.GetAsset(rctx, rec.Source())
	cancel()
	if err != nil {
		return "", "", err
	}
	// A date or location patch is applied by Immich in the background (it
	// re-extracts metadata to recompute localDateTime), so a mismatch right
	// after patching is expected. Poll until it settles; fail only if it
	// never does.
	deadline := time.Now().Add(p.settleTimeout())
	for {
		problem := TimelineMismatch(oldA, newA)
		if problem == "" {
			break
		}
		if time.Now().After(deadline) {
			return "", "", fmt.Errorf("timeline check failed after waiting %s: %s", p.settleTimeout(), problem)
		}
		p.Console.Status(rec.FileName + ": waiting for Immich to apply the date/location patch")
		select {
		case <-ctx.Done():
			return "", "", errInterrupted
		case <-time.After(p.PollInterval):
		}
		rctx, cancel := remoteCtx(ctx)
		newA, err = p.Immich.GetAsset(rctx, rec.NewAssetID)
		cancel()
		if err != nil {
			return "", "", err
		}
	}
	return manifest.Verified, "localDateTime " + oldA.LocalDateTime.Format("2006-01-02 15:04:05") + " and location match", nil
}

// settleTimeout bounds the wait for patched dates and locations to show up.
func (p *Processor) settleTimeout() time.Duration {
	if p.SettleTimeout > 0 {
		return p.SettleTimeout
	}
	return 2 * time.Minute
}

// TimelineMismatch describes how newA differs from oldA in timeline date or
// location, or returns "".
func TimelineMismatch(oldA, newA *immich.Asset) string {
	if d := oldA.LocalDateTime.Sub(newA.LocalDateTime); d < -time.Second || d > time.Second {
		return fmt.Sprintf("localDateTime %s vs original %s", newA.LocalDateTime.Format(time.RFC3339), oldA.LocalDateTime.Format(time.RFC3339))
	}
	oe, ne := exif(oldA), exif(newA)
	if !sameCoord(oe.Latitude, ne.Latitude) || !sameCoord(oe.Longitude, ne.Longitude) {
		return fmt.Sprintf("location %s vs original %s", coord(ne), coord(oe))
	}
	return ""
}

func (p *Processor) trashOriginal(ctx context.Context, rec *manifest.Record) (manifest.State, string, error) {
	// People may have been named since the upload gate ran.
	ok, why, err := p.faceGate(ctx, rec)
	if err != nil {
		return "", "", err
	}
	if !ok {
		rctx, cancel := remoteCtx(ctx)
		defer cancel()
		if err := p.Immich.Trash(rctx, rec.NewAssetID); err != nil {
			return "", "", fmt.Errorf("people were named during processing and trashing the new asset failed: %w", err)
		}
		rec.NewAssetID = ""
		rec.ResumeFrom = manifest.Validated
		return manifest.NeedsConfirm, why, nil
	}
	if rec.People != "" {
		p.Console.Printf("%s: reassign faces for: %s", rec.FileName, rec.People)
	}
	rctx, cancel := remoteCtx(ctx)
	defer cancel()
	if err := p.Immich.Trash(rctx, rec.Source()); err != nil {
		return "", "", err
	}
	what := "original"
	if rec.SourceAssetID != "" {
		what = "previous replacement " + rec.SourceAssetID
	}
	return manifest.Trashed, what + " moved to trash; replaced by " + rec.NewAssetID, nil
}

func (p *Processor) complete(rec *manifest.Record) (manifest.State, string, error) {
	if rec.Mode == manifest.ModeRestore {
		// OutputPath is the backup itself: never delete it.
		rec.OutputPath = ""
		how := "uploaded the backup as " + rec.NewAssetID
		if rec.NewAssetID == rec.AssetID {
			how = "restored the original from the trash"
		}
		return manifest.Restored, how, nil
	}
	if rec.OutputPath != "" && rec.OutputPath != rec.BackupPath {
		if err := os.Remove(rec.OutputPath); err != nil && !os.IsNotExist(err) {
			p.Log.Warn("could not remove work file", "path", rec.OutputPath, "err", err)
		}
	}
	return manifest.Done, fmt.Sprintf("%s → %s crf %d", config.HumanBytes(rec.OriginalSize), config.HumanBytes(rec.NewSize), rec.CRF), nil
}

func exif(a *immich.Asset) api.ExifResponseDto {
	if a.ExifInfo == nil {
		return api.ExifResponseDto{}
	}
	return *a.ExifInfo
}

func strOr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func sameCoord(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return math.Abs(*a-*b) < 1e-5
}

func coord(e api.ExifResponseDto) string {
	if e.Latitude == nil || e.Longitude == nil {
		return "(none)"
	}
	return fmt.Sprintf("%.6f,%.6f", *e.Latitude, *e.Longitude)
}
