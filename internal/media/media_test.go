package media

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func argValue(args []string, flag string) (string, bool) {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return "", false
	}
	return args[i+1], true
}

func sdrProbe() Probe {
	return Probe{HasVid: true, Video: VideoStream{Codec: "h264", PixFmt: "yuv420p", ColorRange: "tv"}, Audio: []AudioStream{{Codec: "aac"}}}
}

func TestTranscodeArgsStandard(t *testing.T) {
	args := TranscodeArgs("in.mp4", "out.tmp", sdrProbe(), MP4, EncodeSettings{CRF: 26, Preset: "slow"})
	want := map[string]string{
		"-c:v": "libx265", "-preset": "slow", "-crf": "26", "-tag:v": "hvc1",
		"-c:a": "copy", "-fps_mode": "passthrough", "-map_metadata": "0",
		"-movflags": "+faststart+use_metadata_tags", "-f": "mp4", "-progress": "pipe:1",
	}
	for k, v := range want {
		if got, _ := argValue(args, k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-map 0:v:0 -map 0:a?") {
		t.Errorf("missing stream maps: %s", joined)
	}
	if strings.Contains(joined, "in_range=full") || strings.Contains(joined, "-color_range") {
		t.Errorf("limited-range source got full-range args: %s", joined)
	}
	if args[len(args)-1] != "out.tmp" || args[slices.Index(args, "-i")+1] != "in.mp4" {
		t.Errorf("bad in/out placement: %s", joined)
	}
}

func TestTranscodeArgsFullRange(t *testing.T) {
	for _, mod := range []func(*Probe){
		func(p *Probe) { p.Video.PixFmt = "yuvj420p" },
		func(p *Probe) { p.Video.ColorRange = "pc" },
	} {
		p := sdrProbe()
		mod(&p)
		args := TranscodeArgs("in.mp4", "out.tmp", p, MP4, EncodeSettings{CRF: 24, Preset: "medium"})
		if vf, _ := argValue(args, "-vf"); vf != "scale=in_range=full:out_range=full,format=yuv420p" {
			t.Errorf("pix_fmt %s range %s: -vf = %q", p.Video.PixFmt, p.Video.ColorRange, vf)
		}
		if cr, _ := argValue(args, "-color_range"); cr != "pc" {
			t.Errorf("-color_range = %q", cr)
		}
		if _, ok := argValue(args, "-pix_fmt"); ok {
			t.Errorf("full-range args should set format in -vf only")
		}
		if crf, _ := argValue(args, "-crf"); crf != "24" {
			t.Errorf("crf = %q", crf)
		}
	}
}

func TestTranscodeArgsAudio(t *testing.T) {
	p := sdrProbe()
	p.Audio = []AudioStream{{Codec: "pcm_s16le"}}
	if a, _ := argValue(TranscodeArgs("i", "o", p, MP4, EncodeSettings{CRF: 26, Preset: "slow"}), "-c:a"); a != "aac" {
		t.Errorf("pcm into mp4: -c:a = %q, want aac", a)
	}
	if a, _ := argValue(TranscodeArgs("i", "o", p, MOV, EncodeSettings{CRF: 26, Preset: "slow"}), "-c:a"); a != "copy" {
		t.Errorf("pcm into mov: -c:a = %q, want copy", a)
	}
}

func TestContainerAndName(t *testing.T) {
	cases := []struct{ in, out string }{
		{"PXL_20240101_120000.mp4", "PXL_20240101_120000.mp4"},
		{"IMG_0001.MOV", "IMG_0001.MOV"},
		{"clip.mkv", "clip.mp4"},
		{"old.3gp", "old.mp4"},
		{"noext", "noext.mp4"},
	}
	for _, c := range cases {
		if got := OutputName(c.in, ContainerFor(c.in)); got != c.out {
			t.Errorf("%s → %s, want %s", c.in, got, c.out)
		}
	}
}

const probeJSON = `{
 "streams": [
  {"codec_type":"video","codec_name":"h264","profile":"High","pix_fmt":"yuvj420p","color_range":"pc",
   "width":1920,"height":1080,"avg_frame_rate":"30000/1001","r_frame_rate":"30/1","bit_rate":"20000000",
   "bits_per_raw_sample":"8","disposition":{"attached_pic":0},
   "side_data_list":[{"side_data_type":"Display Matrix","rotation":-90}]},
  {"codec_type":"audio","codec_name":"aac"},
  {"codec_type":"video","codec_name":"mjpeg","width":320,"height":240,"disposition":{"attached_pic":1}}
 ],
 "format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"12.345","size":"30000000","bit_rate":"20100000"}
}`

func TestParseProbe(t *testing.T) {
	p, err := ParseProbe([]byte(probeJSON))
	if err != nil {
		t.Fatal(err)
	}
	v := p.Video
	if v.Codec != "h264" || v.Width != 1920 || v.Height != 1080 {
		t.Fatalf("video = %+v", v)
	}
	if v.Rotation != 270 {
		t.Errorf("rotation = %d, want 270", v.Rotation)
	}
	if w, h := v.DisplaySize(); w != 1080 || h != 1920 {
		t.Errorf("display = %dx%d", w, h)
	}
	if !v.FullRange() || v.HighBitDepthOrHDR() {
		t.Errorf("full=%v hdr=%v", v.FullRange(), v.HighBitDepthOrHDR())
	}
	if p.Duration != 12.345 || len(p.Audio) != 1 {
		t.Errorf("duration %v audio %d", p.Duration, len(p.Audio))
	}
	// 20 Mbit/s / (1920*1080*29.97) ≈ 0.3217
	if bpp := p.BPP(); bpp < 0.321 || bpp > 0.323 {
		t.Errorf("bpp = %v", bpp)
	}
}

func TestParseProbeRotateTag(t *testing.T) {
	p, err := ParseProbe([]byte(`{"streams":[{"codec_type":"video","codec_name":"h264","width":640,"height":480,"tags":{"rotate":"90"}}],"format":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Video.Rotation != 270 {
		t.Errorf("rotate tag 90 (clockwise) → %d, want 270", p.Video.Rotation)
	}
}

func TestParseProbeNoVideo(t *testing.T) {
	if _, err := ParseProbe([]byte(`{"streams":[{"codec_type":"audio","codec_name":"aac"}],"format":{}}`)); err == nil {
		t.Fatal("expected error")
	}
}

func TestParseProgress(t *testing.T) {
	in := "frame=10\nout_time_us=1500000\nspeed=2.5x\nprogress=continue\nout_time_us=3000000\nspeed=2.4x\nprogress=end\n"
	var got []Progress
	parseProgress(strings.NewReader(in), func(p Progress) { got = append(got, p) })
	if len(got) != 2 || got[0].OutTime != 1500*time.Millisecond || got[1].Speed != "2.4x" || !got[1].Done {
		t.Fatalf("got %+v", got)
	}
}

func TestCompareMeta(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	a := Meta{CreateDate: "2021:06:01 10:20:30", GPSLatitude: f(47.6), GPSLongitude: f(-122.3)}
	if d := CompareMeta(a, a); d != "" {
		t.Fatal(d)
	}
	b := a
	b.GPSLatitude = nil
	if CompareMeta(a, b) == "" {
		t.Error("lost GPS not detected")
	}
	c := a
	c.CreateDate = "2021:06:01 10:20:31"
	if CompareMeta(a, c) == "" {
		t.Error("changed date not detected")
	}
}

func TestTranscodeArgsKeepSourceTimeBase(t *testing.T) {
	args := TranscodeArgs("i", "o", sdrProbe(), MP4, EncodeSettings{CRF: 26, Preset: "slow"})
	if tb, _ := argValue(args, "-enc_time_base"); tb != "-1" {
		t.Errorf("default -enc_time_base = %q, want -1", tb)
	}
	args = TranscodeArgs("i", "o", sdrProbe(), MP4, EncodeSettings{CRF: 26, Preset: "slow", SourceTimeBase: "demux"})
	if tb, _ := argValue(args, "-enc_time_base"); tb != "demux" {
		t.Errorf("ffmpeg 7 -enc_time_base = %q, want demux", tb)
	}
	// It is an output option: it must come after the input.
	if slices.Index(args, "-enc_time_base") < slices.Index(args, "-i") {
		t.Error("-enc_time_base placed before -i")
	}
}
