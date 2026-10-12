package voice

import (
	"fmt"
	"strconv"
)

// frameMs is the packet duration ffmpeg is told to produce. Discord's voice
// pipeline expects roughly this per packet, and a fixed size means the sink
// can count position in packets without parsing opus headers.
const frameMs = 20

// ffmpegArgs builds the command that turns a cached file into 20ms Ogg/Opus
// frames on stdout: 48kHz stereo is exactly what Discord's voice servers take,
// and re-encoding through libopus guarantees the frame size whatever the
// source was.
func ffmpegArgs(path string, positionMs int64) []string {
	args := []string{
		"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
	}
	if positionMs > 0 {
		args = append(args, "-ss", formatSeconds(positionMs))
	}
	return append(args,
		"-i", path,
		"-vn",
		"-ar", "48000",
		"-ac", "2",
		"-c:a", "libopus",
		"-b:a", "192k",
		"-frame_duration", strconv.Itoa(frameMs),
		"-f", "opus",
		"pipe:1",
	)
}

// formatSeconds renders milliseconds the way ffmpeg's -ss wants: seconds with
// a fraction, no exponent.
func formatSeconds(ms int64) string {
	return strconv.FormatFloat(float64(ms)/1000, 'f', 3, 64)
}

// command renders a launch for logs.
func command(bin string, args []string) string {
	out := bin
	for _, a := range args {
		out += fmt.Sprintf(" %q", a)
	}
	return out
}
