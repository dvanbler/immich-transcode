package media

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
)

// CopyMetadataArgs copies QuickTime-family metadata (creation date, GPS,
// make, model, Apple keys) from orig into out in place.
//
// Rotation is excluded: ffmpeg has already applied it to the pixels, and
// copying the track matrix back would rotate the video twice.
//
// ffmpeg turns a "location" tag into a 3GPP loci box whose 16.16 fixed-point
// coordinates are off by ~1 m, and exiftool's composite GPS (which Immich
// reads) prefers it. It is deleted before the copy, so it survives only if
// the original had one.
func CopyMetadataArgs(orig, out string) []string {
	return []string{
		"-api", "LargeFileSupport=1",
		"-UserData:LocationInformation=",
		"-TagsFromFile", orig,
		"-QuickTime:All", "-Keys:All", "-UserData:All",
		"--Rotation",
		"-overwrite_original",
		out,
	}
}

// CopyMetadata runs exiftool with CopyMetadataArgs.
func (t Tools) CopyMetadata(ctx context.Context, orig, out string) error {
	_, _, err := runCapture(ctx, t.ExifTool, CopyMetadataArgs(orig, out)...)
	return err
}

// Meta is the timeline-relevant metadata exiftool reads from a video.
type Meta struct {
	CreateDate   string   `json:"CreateDate"`
	CreationDate string   `json:"CreationDate"` // Apple Keys tag, includes the zone offset
	GPSLatitude  *float64 `json:"GPSLatitude"`
	GPSLongitude *float64 `json:"GPSLongitude"`
}

// ReadMeta reads Meta from path.
func (t Tools) ReadMeta(ctx context.Context, path string) (Meta, error) {
	out, _, err := runCapture(ctx, t.ExifTool, "-api", "LargeFileSupport=1", "-j", "-n",
		"-QuickTime:CreateDate", "-Keys:CreationDate", "-GPSLatitude", "-GPSLongitude", path)
	if err != nil {
		return Meta{}, err
	}
	var rows []struct {
		Meta
		// exiftool -n prints dates as strings but may emit 0 for an unset
		// CreateDate; accept either.
		CreateDate   any `json:"CreateDate"`
		CreationDate any `json:"CreationDate"`
	}
	if err := json.Unmarshal(out, &rows); err != nil || len(rows) != 1 {
		return Meta{}, fmt.Errorf("parse exiftool output: %v", err)
	}
	m := rows[0].Meta
	m.CreateDate = stringify(rows[0].CreateDate)
	m.CreationDate = stringify(rows[0].CreationDate)
	return m, nil
}

func stringify(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	default:
		return fmt.Sprint(x)
	}
}

// CompareMeta reports the first difference between orig and out, or "".
func CompareMeta(orig, out Meta) string {
	if orig.CreateDate != out.CreateDate {
		return fmt.Sprintf("CreateDate %q became %q", orig.CreateDate, out.CreateDate)
	}
	if orig.CreationDate != out.CreationDate {
		return fmt.Sprintf("CreationDate %q became %q", orig.CreationDate, out.CreationDate)
	}
	if !sameCoord(orig.GPSLatitude, out.GPSLatitude) || !sameCoord(orig.GPSLongitude, out.GPSLongitude) {
		return fmt.Sprintf("GPS %s became %s", fmtCoord(orig), fmtCoord(out))
	}
	return ""
}

func sameCoord(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return math.Abs(*a-*b) < 1e-5
}

func fmtCoord(m Meta) string {
	if m.GPSLatitude == nil || m.GPSLongitude == nil {
		return "(none)"
	}
	return fmt.Sprintf("%.6f,%.6f", *m.GPSLatitude, *m.GPSLongitude)
}
