package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
)

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

// rebaseToZero rewraps a single-stream media file through its own stream-copy
// remux so its first packet lands at timestamp zero, returning the new path or
// "" on failure.
//
// This is needed because Crunchyroll's video track carries a presentation
// time offset as its first packet timestamp (observed: 8.889s on a 24-minute
// episode, sometimes 0 depending on the playback session), while audio and
// subtitles start at zero. A single-input ffmpeg invocation strips that start
// offset from the copied packets — but the multi-input merge cannot: there
// ffmpeg strips only the global minimum start across ALL inputs, which leaves
// the video offset in place against an audio track that already starts at
// zero (packet-level verified both ways). Rebasing every track on its own
// first therefore makes the merge offset-free, without re-encoding anything.
func rebaseToZero(path, selector string) string {
	out := path + ".rebased.mkv"
	cmd := exec.Command(ffmpegPath, "-y", "-v", "error", "-i", path,
		"-map", "0:"+selector, "-c", "copy", out)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		_ = os.Remove(out)
		fmt.Printf("! Could not rebase %s to a zero start: %v\n%s\n", path, err, stderr.String())
		return ""
	}
	return out
}

// mergeEverything merges the video, all audio tracks and all subtitle tracks
// into a single MKV container.
func mergeEverything(videoFile string, audioTracks, subTracks []mediaTrack, outputFile string, info EpisodeInfo) {
	// Every media input is rebased to a zero start on its own first (see
	// rebaseToZero): the multi-input merge below would otherwise keep the
	// video track's Crunchyroll presentation-time offset in place while the
	// audio starts at zero, displacing the picture against sound. The rebased
	// intermediates are stream copies and are removed once merged.
	rebased := make([]string, 0, 1+len(audioTracks))
	defer func() {
		for _, p := range rebased {
			_ = os.Remove(p)
		}
	}()
	videoInput := videoFile
	if r := rebaseToZero(videoFile, "v:0"); r != "" {
		rebased = append(rebased, r)
		videoInput = r
	}
	audioInputs := make([]string, len(audioTracks))
	for i, audio := range audioTracks {
		if r := rebaseToZero(audio.file, "a:0"); r != "" {
			rebased = append(rebased, r)
			audioInputs[i] = r
			continue
		}
		audioInputs[i] = audio.file
	}

	args := []string{"-i", videoInput}

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
