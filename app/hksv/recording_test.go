package hksv

import (
	"strings"
	"testing"
)

// argValue returns the value following the first occurrence of flag in args.
func argValue(args []string, flag string) (string, bool) {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag {
			return args[i+1], true
		}
	}
	return "", false
}

func TestFFmpegArgsReencodesWithAlignedKeyframes(t *testing.T) {
	r := newRecorder("cam", "ffmpeg", false, func(int) (string, error) { return "", nil })
	cfg := SelectedConfig{
		FragmentLengthMS: 4000,
		Video: VideoSelection{
			Profile: H264ProfileMain, Level: H264Level40,
			Width: 1920, Height: 1080, FrameRate: 30, BitrateKbps: 2000,
		},
	}
	args := r.ffmpegArgs(cfg, false, "rtsps://x")

	if v, _ := argValue(args, "-c:v"); v != "libx264" {
		t.Fatalf("video must be re-encoded, got -c:v %q", v)
	}
	if v, _ := argValue(args, "-profile:v"); v != "main" {
		t.Fatalf("profile = %q, want main", v)
	}
	if v, _ := argValue(args, "-level:v"); v != "4.0" {
		t.Fatalf("level = %q, want 4.0", v)
	}
	if v, _ := argValue(args, "-bf"); v != "0" {
		t.Fatalf("-bf = %q, want 0 (no B-frames)", v)
	}
	// GOP must equal fps * fragmentSeconds so keyframes land on fragment edges.
	if v, _ := argValue(args, "-g"); v != "120" {
		t.Fatalf("-g = %q, want 120 (30fps * 4s)", v)
	}
	if v, ok := argValue(args, "-force_key_frames"); !ok || !strings.Contains(v, "n_forced*4") {
		t.Fatalf("-force_key_frames = %q, want expr aligned to 4s", v)
	}
	if v, _ := argValue(args, "-b:v"); v != "2000k" {
		t.Fatalf("-b:v = %q, want 2000k", v)
	}
	if v, _ := argValue(args, "-preset"); v == "ultrafast" {
		t.Fatal("preset must not be ultrafast: it forces CAVLC and downgrades H.264 to Baseline, which HKSV rejects when Main was negotiated")
	}
	if !strings.Contains(strings.Join(args, " "), "frag_keyframe") {
		t.Fatal("movflags must include frag_keyframe")
	}
}

func TestFFmpegArgsAudioLC(t *testing.T) {
	r := newRecorder("cam", "ffmpeg", false, func(int) (string, error) { return "", nil })
	cfg := SelectedConfig{
		FragmentLengthMS: 4000,
		Video:            VideoSelection{Width: 1280, Height: 720, FrameRate: 30},
		Audio:            AudioSelection{SampleRate: SampleRate16kHz, Channels: 1, MaxBitrate: 32},
	}
	args := r.ffmpegArgs(cfg, true, "rtsps://x")

	if v, _ := argValue(args, "-c:a"); v != "aac" {
		t.Fatalf("recording audio must be AAC-LC, got -c:a %q", v)
	}
	if v, _ := argValue(args, "-ar"); v != "16000" {
		t.Fatalf("-ar = %q, want 16000", v)
	}
}

func TestH264ProfileLevelNames(t *testing.T) {
	if got := h264ProfileName(H264ProfileHigh); got != "high" {
		t.Fatalf("high profile = %q", got)
	}
	if got := h264ProfileName(H264ProfileBaseline); got != "baseline" {
		t.Fatalf("baseline profile = %q", got)
	}
	if got := h264LevelName(H264Level31); got != "3.1" {
		t.Fatalf("level 3.1 = %q", got)
	}
	if got := h264LevelName(H264Level32); got != "3.2" {
		t.Fatalf("level 3.2 = %q", got)
	}
}
