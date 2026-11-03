package main

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// startOffsetThreshold is the smallest track start timestamp that gets
// compensated. Some Crunchyroll manifests carry a non-zero presentation time
// offset for the video track; the decrypted MP4 keeps that offset as its first
// timestamp, and `-c:v copy` then carries it into the MKV — leaving the video
// track displaced by seconds (observed: 8.889s) against audio and subtitles
// that both start at zero. Viewers see constant audio/video desync and a
// silent video-only tail as long as the offset. Anything below the threshold
// is float noise and is left untouched.
const startOffsetThreshold = 0.01

// mediaStartOffset reports the start timestamp of the first stream matching
// selector (e.g. "v:0", "a:0") in path via ffprobe. It returns 0 whenever
// ffprobe is unavailable or the value is unusable, so the merge degrades to
// the uncompensated behavior instead of failing.
func mediaStartOffset(path, selector string) float64 {
	ffprobe := findFFprobe()
	if ffprobe == "" || path == "" {
		return 0
	}
	out, err := exec.Command(ffprobe, "-v", "error", "-select_streams", selector,
		"-show_entries", "stream=start_time", "-of", "csv=p=0", path).Output()
	if err != nil {
		return 0
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return 0
	}
	value, err := strconv.ParseFloat(strings.Fields(line)[0], 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return value
}

// mediaTrack pairs a downloaded temporary file with the locale it represents.
type mediaTrack struct {
	file   string
	locale string
	format string // subtitle source format, e.g. "ass" or "vtt". Unused for audio tracks.
	isCC   bool   // true if this track is a closed caption rather than a regular subtitle
}

// trackTitle returns a human-readable track name for a locale, falling back to
// the raw locale when it isn't in the known list.
func trackTitle(locale string) string {
	if name, ok := languageNames[locale]; ok {
		return name
	}
	return locale
}

// mergeEverything merges the video, all audio tracks and all subtitle tracks
// into a single MKV container.
func mergeEverything(videoFile string, audioTracks, subTracks []mediaTrack, outputFile string, info EpisodeInfo) {
	// Compensate per-input start timestamps so every track begins at zero.
	// `-c copy` preserves each input's first timestamp, so a video MP4 that
	// carries Crunchyroll's presentation time offset would stay displaced
	// against the audio inside the merged MKV; `-itsoffset` shifts only that
	// input back to zero without re-encoding anything.
	offsets := make([]float64, 1+len(audioTracks))
	offsets[0] = mediaStartOffset(videoFile, "v:0")
	for i, audio := range audioTracks {
		offsets[1+i] = mediaStartOffset(audio.file, "a:0")
	}

	args := []string{}
	addInput := func(path string, offset float64) {
		if offset > startOffsetThreshold || offset < -startOffsetThreshold {
			args = append(args, "-itsoffset", fmt.Sprintf("%.3f", -offset))
		}
		args = append(args, "-i", path)
	}
	addInput(videoFile, offsets[0])
	for i, audio := range audioTracks {
		addInput(audio.file, offsets[1+i])
	}
	for _, sub := range subTracks {
		args = append(args, "-i", sub.file)
	}

	// Map every input stream explicitly; without this ffmpeg keeps only one
	// stream of each type.
	args = append(args, "-map", "0:v:0")
	for i := range audioTracks {
		args = append(args, "-map", fmt.Sprintf("%d:a:0", 1+i))
	}
	for j := range subTracks {
		args = append(args, "-map", fmt.Sprintf("%d", 1+len(audioTracks)+j))
	}

	args = append(args, "-c:v", "copy", "-c:a", "copy")
	// WebVTT cue-positioning metadata (e.g. "line:90% align:center") isn't
	// rendered correctly by most players when copied as-is into an MKV, so
	// VTT subtitle/caption tracks are transcoded to SRT (which has no
	// positioning syntax) instead of stream-copied. ASS tracks keep "copy"
	// to preserve their styling.
	for j, sub := range subTracks {
		if sub.format == "vtt" {
			args = append(args, fmt.Sprintf("-c:s:%d", j), "srt")
		} else {
			args = append(args, fmt.Sprintf("-c:s:%d", j), "copy")
		}
	}

	for i, audio := range audioTracks {
		args = append(args,
			fmt.Sprintf("-metadata:s:a:%d", i), "language="+languageCodes[audio.locale],
			fmt.Sprintf("-metadata:s:a:%d", i), "title="+trackTitle(audio.locale),
		)
	}
	for j, sub := range subTracks {
		title := trackTitle(sub.locale)
		if sub.isCC {
			title += " [CC]"
		}
		args = append(args,
			fmt.Sprintf("-metadata:s:s:%d", j), "language="+languageCodes[sub.locale],
			fmt.Sprintf("-metadata:s:s:%d", j), "title="+title,
		)
	}
	// Mark only the first audio track and the first non-CC subtitle track as
	// default. Disposition must be set on every track: each downloaded audio
	// file is a standalone default stream, so the non-primary ones must be
	// explicitly cleared. Closed captions are never marked default; they're
	// an opt-in extra a viewer toggles deliberately.
	for i := range audioTracks {
		disposition := "0"
		if i == 0 {
			disposition = "default"
		}
		args = append(args, fmt.Sprintf("-disposition:a:%d", i), disposition)
	}
	defaultSubSet := false
	for j, sub := range subTracks {
		disposition := "0"
		if !sub.isCC && !defaultSubSet {
			disposition = "default"
			defaultSubSet = true
		}
		args = append(args, fmt.Sprintf("-disposition:s:%d", j), disposition)
	}

	args = append(args,
		"-metadata:g", "title="+fmt.Sprintf("S%02vE%02v - %s", info.EpisodeMetadata.SeasonNumber, info.EpisodeMetadata.EpisodeNumber, info.Title),
		"-metadata:g", "show="+info.EpisodeMetadata.SeriesTitle,
		"-metadata:g", "track="+fmt.Sprintf("%v", info.EpisodeMetadata.EpisodeNumber),
		"-metadata:g", "season_number="+fmt.Sprintf("%v", info.EpisodeMetadata.SeasonNumber),
		outputFile,
	)

	cmd := exec.Command(ffmpegPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		_ = os.Remove(outputFile)
		panic(fmt.Sprintf("ffmpeg failed: %s\n%s", err, stderr.String()))
	}

	// Remove temporary files
	_ = os.Remove(videoFile)
	for _, audio := range audioTracks {
		_ = os.Remove(audio.file)
	}
	for _, sub := range subTracks {
		_ = os.Remove(sub.file)
	}

	fmt.Printf("\nDownload finished! Output file: %s\n\n", outputFile)
}
