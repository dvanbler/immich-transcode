package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"immich-transcode/internal/config"
	"immich-transcode/internal/immich"
	"immich-transcode/internal/manifest"
	"immich-transcode/internal/media"
	"immich-transcode/internal/selection"
)

type ioWriter = io.Writer

// BackupPath is backup/YYYY/MM/<assetId>_<originalFileName>, dated by the
// asset's timeline date.
func BackupPath(root string, a *immich.Asset) string {
	t := a.LocalDateTime
	return filepath.Join(root, fmt.Sprintf("%04d", t.Year()), fmt.Sprintf("%02d", int(t.Month())),
		a.Id.String()+"_"+sanitizeName(a.OriginalFileName))
}

func sanitizeName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, `\`, "/"))
	r := strings.NewReplacer("/", "_", ":", "_", "*", "_", "?", "_", `"`, "_", "<", "_", ">", "_", "|", "_")
	name = r.Replace(name)
	if name == "" || name == "." || name == ".." {
		name = "original"
	}
	return name
}

// download writes the original to the backup directory and verifies it
// against Immich's checksum. An existing verified backup is reused; an
// existing backup that does not match is never overwritten.
func (p *Processor) download(ctx context.Context, rec *manifest.Record, asset *immich.Asset) (manifest.State, string, error) {
	path := BackupPath(p.Cfg.BackupDir, asset)
	rec.BackupPath = path
	if exists(path) {
		_, sum, err := immich.FileSHA1(path)
		if err != nil {
			return "", "", err
		}
		if sum != asset.Checksum {
			return "", "", fmt.Errorf("backup %s exists but its checksum %s does not match Immich's %s; move it aside to re-download", path, sum, asset.Checksum)
		}
		fi, _ := os.Stat(path)
		rec.OriginalSize = fi.Size()
		return manifest.Downloaded, "existing backup verified", nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", "", err
	}
	part := path + ".part"
	f, err := os.Create(part)
	if err != nil {
		return "", "", err
	}
	total := fileSize(asset)
	var got atomic.Int64
	stop := p.ticker(func() string {
		return fmt.Sprintf("%s: downloading %s", rec.FileName, progressText(got.Load(), total))
	})
	sum, n, err := p.Immich.Download(ctx, rec.AssetID, f, func(b int64) { got.Add(b) })
	stop()
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && sum != asset.Checksum {
		err = fmt.Errorf("downloaded checksum %s does not match Immich's %s", sum, asset.Checksum)
	}
	if err != nil {
		os.Remove(part)
		return "", "", err
	}
	if err := os.Rename(part, path); err != nil {
		return "", "", err
	}
	rec.OriginalSize = n
	return manifest.Downloaded, fmt.Sprintf("%s verified", config.HumanBytes(n)), nil
}

func (p *Processor) decodeCheck(ctx context.Context, rec *manifest.Record) (manifest.State, string, error) {
	p.Console.Status(rec.FileName + ": checking original decodes cleanly")
	problems, err := p.Tools.DecodeCheck(ctx, rec.BackupPath)
	if err != nil {
		return "", "", err
	}
	if problems != "" {
		rec.Detail = problems
		return manifest.SkippedCorrupt, firstLine(problems), nil
	}
	return manifest.DecodeOK, "decodes cleanly", nil
}

func (p *Processor) probe(ctx context.Context, rec *manifest.Record) (manifest.State, string, error) {
	pr, err := p.Tools.ProbeFile(ctx, rec.BackupPath)
	if err != nil {
		return manifest.SkippedOther, err.Error(), nil
	}
	verdict, why := selection.Decide(pr, p.Cfg.BPP)
	rec.Detail = why
	switch verdict {
	case selection.SkipCodec:
		return manifest.SkippedCodec, why, nil
	case selection.SkipEfficient:
		return manifest.SkippedEff, why, nil
	case selection.SkipHDR:
		return manifest.SkippedHDR, why, nil
	}
	return manifest.Probed, why, nil
}

func (p *Processor) transcode(ctx context.Context, rec *manifest.Record, asset *immich.Asset) (manifest.State, string, error) {
	pr, err := p.Tools.ProbeFile(ctx, rec.BackupPath)
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(p.Cfg.WorkDir, 0o755); err != nil {
		return "", "", err
	}
	c := media.ContainerFor(asset.OriginalFileName)
	out := filepath.Join(p.Cfg.WorkDir, rec.AssetID+c.Ext)
	tmp := filepath.Join(p.Cfg.WorkDir, rec.AssetID+".partial"+c.Ext)
	crf := p.Opts.CRF
	args := media.TranscodeArgs(rec.BackupPath, tmp, pr, c, media.EncodeSettings{CRF: crf, Preset: p.Cfg.Preset, SourceTimeBase: p.Tools.SourceTimeBase()})
	p.Log.Debug("ffmpeg", "asset", rec.AssetID, "args", args)

	start := time.Now()
	err = p.Tools.Transcode(ctx, args, func(pg media.Progress) {
		frac := 0.0
		if pr.Duration > 0 {
			frac = math.Min(1, pg.OutTime.Seconds()/pr.Duration)
		}
		eta := ""
		if el := time.Since(start); frac > 0.01 {
			eta = " ETA " + (time.Duration(float64(el)/frac) - el).Round(time.Second).String()
		}
		p.Console.Status(fmt.Sprintf("%s: encoding %s %5.1f%% speed %s%s",
			rec.FileName, bar(frac, 20), frac*100, pg.Speed, eta))
	})
	if err != nil {
		os.Remove(tmp)
		if ctx.Err() != nil {
			return "", "", errInterrupted
		}
		return "", "", err
	}
	if err := os.Rename(tmp, out); err != nil {
		return "", "", err
	}
	rec.OutputPath = out
	rec.CRF = crf
	fi, _ := os.Stat(out)
	rec.NewSize = fi.Size()
	return manifest.Transcoded, fmt.Sprintf("crf %d preset %s in %s", crf, p.Cfg.Preset, time.Since(start).Round(time.Second)), nil
}

func (p *Processor) copyMetadata(ctx context.Context, rec *manifest.Record) (manifest.State, string, error) {
	p.Console.Status(rec.FileName + ": copying metadata")
	if err := p.Tools.CopyMetadata(ctx, rec.BackupPath, rec.OutputPath); err != nil {
		return "", "", err
	}
	return manifest.MetadataCopied, "exiftool copy", nil
}

// validate checks the output against the original. A too-small saving is a
// skip, not a failure; anything else that fails is an error.
func (p *Processor) validate(ctx context.Context, rec *manifest.Record) (manifest.State, string, error) {
	p.Console.Status(rec.FileName + ": validating output")
	fi, err := os.Stat(rec.OutputPath)
	if err != nil {
		return "", "", err
	}
	rec.NewSize = fi.Size()
	saved := pct(rec.OriginalSize, rec.NewSize)
	if saved < p.Cfg.MinSavingsPercent {
		os.Remove(rec.OutputPath)
		return manifest.SkippedLowGain, fmt.Sprintf("only %.1f%% smaller (%s → %s), need %.0f%%",
			saved, config.HumanBytes(rec.OriginalSize), config.HumanBytes(rec.NewSize), p.Cfg.MinSavingsPercent), nil
	}
	if err := ValidateOutput(ctx, p.Tools, rec.BackupPath, rec.OutputPath); err != nil {
		return "", "", err
	}
	return manifest.Validated, fmt.Sprintf("%s → %s (%.1f%% saved)",
		config.HumanBytes(rec.OriginalSize), config.HumanBytes(rec.NewSize), saved), nil
}

// ValidateOutput runs every output check that does not involve Immich.
func ValidateOutput(ctx context.Context, t media.Tools, orig, out string) error {
	problems, err := t.DecodeCheck(ctx, out)
	if err != nil {
		return err
	}
	if problems != "" {
		return fmt.Errorf("output does not decode cleanly: %s", firstLine(problems))
	}
	po, err := t.ProbeFile(ctx, orig)
	if err != nil {
		return err
	}
	pn, err := t.ProbeFile(ctx, out)
	if err != nil {
		return err
	}
	if pn.Video.Codec != "hevc" {
		return fmt.Errorf("output codec is %s, not hevc", pn.Video.Codec)
	}
	if d := math.Abs(po.Duration - pn.Duration); d > 0.5 {
		return fmt.Errorf("duration %.3fs differs from original %.3fs", pn.Duration, po.Duration)
	}
	ow, oh := po.Video.DisplaySize()
	nw, nh := pn.Video.DisplaySize()
	if ow != nw || oh != nh {
		return fmt.Errorf("displayed size %dx%d (rotation %d) differs from original %dx%d (rotation %d)",
			nw, nh, pn.Video.Rotation, ow, oh, po.Video.Rotation)
	}
	if len(pn.Audio) != len(po.Audio) {
		return fmt.Errorf("output has %d audio streams, original %d", len(pn.Audio), len(po.Audio))
	}
	co, err := t.CountVideoPackets(ctx, orig)
	if err != nil {
		return err
	}
	cn, err := t.CountVideoPackets(ctx, out)
	if err != nil {
		return err
	}
	if co != cn {
		return fmt.Errorf("video packet count %d differs from original %d", cn, co)
	}
	do, err := t.DuplicateTimestamps(ctx, orig)
	if err != nil {
		return err
	}
	dn, err := t.DuplicateTimestamps(ctx, out)
	if err != nil {
		return err
	}
	if dn > do {
		return fmt.Errorf("%d video frames share a timestamp with another frame (original: %d)", dn, do)
	}
	mo, err := t.ReadMeta(ctx, orig)
	if err != nil {
		return err
	}
	mn, err := t.ReadMeta(ctx, out)
	if err != nil {
		return err
	}
	if diff := media.CompareMeta(mo, mn); diff != "" {
		return errors.New("metadata mismatch: " + diff)
	}
	return nil
}

// ticker refreshes a status line every half second until stop is called.
func (p *Processor) ticker(text func() string) (stop func()) {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			p.Console.Status(text())
			select {
			case <-done:
				return
			case <-t.C:
			}
		}
	}()
	return func() { close(done); <-finished }
}

func progressText(got, total int64) string {
	if total <= 0 {
		return config.HumanBytes(got)
	}
	frac := math.Min(1, float64(got)/float64(total))
	return fmt.Sprintf("%s %5.1f%% of %s", bar(frac, 20), frac*100, config.HumanBytes(total))
}

func bar(frac float64, width int) string {
	n := int(frac * float64(width))
	return "[" + strings.Repeat("#", n) + strings.Repeat("-", width-n) + "]"
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}
