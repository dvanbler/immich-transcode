package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"immich-transcode/internal/manifest"
	"immich-transcode/internal/media"
)

// VerifyReplacement downloads the replacement of a done asset from Immich
// and runs the pre-upload validation again, against the local backup of
// the original. It returns a description of what is wrong with the
// replacement (problem), or an error when the check itself could not run
// (missing backup, download failure).
func VerifyReplacement(ctx context.Context, im Immich, tools media.Tools, rec manifest.Record, workDir string, progress func(int64)) (problem string, err error) {
	if rec.State != manifest.Done {
		return "", fmt.Errorf("asset is %s, not done", rec.State)
	}
	if rec.NewAssetID == "" {
		return "", errors.New("no replacement asset recorded")
	}
	if !exists(rec.BackupPath) {
		return "", fmt.Errorf("backup %s is missing", rec.BackupPath)
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return "", err
	}
	tmp := filepath.Join(workDir, "verify-"+rec.NewAssetID+filepath.Ext(rec.FileName))
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp)
	_, n, err := im.Download(ctx, rec.NewAssetID, f, progress)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("download replacement %s: %w", rec.NewAssetID, err)
	}
	if rec.NewSize > 0 && n != rec.NewSize {
		return fmt.Sprintf("replacement is %d bytes in Immich but %d bytes were uploaded", n, rec.NewSize), nil
	}
	if err := ValidateOutput(ctx, tools, rec.BackupPath, tmp); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return err.Error(), nil
	}
	return "", nil
}
