package voice

import (
	"fmt"
	"strconv"
)

// FrameMs is the packet duration ffmpeg is told to produce, and the unit a
// buffer is counted in. Discord's voice
// pipeline expects roughly this per packet, and a fixed size means the sink
// can count position in packets without parsing opus headers.
const frameMs = 20

// FrameMs is frameMs for callers that size a buffer in milliseconds.
const FrameMs = frameMs

// DefaultVolume is how loud the bot plays when the config does not say. Music
// arrives mastered hot, and Discord's opus is not the place to find headroom:
// an eighth keeps a full-scale mix well clear of the ceiling, which is where
// the peaks were coming from, and leaves the limiter as the backstop rather
// than the thing doing the work on every song. It is config.DefaultVolume; this
// is the copy the encoder needs so a zero volume is still a number.
const DefaultVolume = 0.125

// defaultBitrateKbps is what the encoder targets when the channel says
// nothing: comfortably above the 64kbps a plain channel allows, well inside
// what a boosted one does.
const defaultBitrateKbps = 192

// maxVoiceBitrateKbps is Discord's ceiling for a voice channel.
const maxVoiceBitrateKbps = 256

// MaxVoiceBitrateKbps is maxVoiceBitrateKbps for callers that clamp.
const MaxVoiceBitrateKbps = maxVoiceBitrateKbps

// ffmpegArgs builds the command that turns a cached file into 20ms Ogg/Opus
// frames on stdout: 48kHz stereo is exactly what Discord's voice servers take,
// and re-encoding through libopus guarantees the frame size whatever the
// source was. The bitrate is the channel's own — Discord caps a voice channel
// there, so matching it is both the loudest and the highest quality the
// channel can carry; the music application is what libopus wants for songs
// rather than speech.
//
// The gain is followed by a limiter, and that order is the point: a gain is a
// multiplier, and a master that already peaks at full scale (or one whose
// decoded peaks sit above it) comes out the other side above it again, which
// clips when the encoder takes it. Three quarters of a hot master is a hope;
// the limiter is the guarantee, whichever way the source is mastered. Level is
// disabled so the limiter does not push the peaks back up to full scale after
// ducking them, which is the same clipping with extra pumping.
func ffmpegArgs(path string, positionMs int64, bitrateKbps int, volume float64) []string {
	args := []string{
		"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
	}
	if positionMs > 0 {
		args = append(args, "-ss", formatSeconds(positionMs))
	}
	if bitrateKbps <= 0 {
		bitrateKbps = defaultBitrateKbps
	}
	if bitrateKbps > maxVoiceBitrateKbps {
		bitrateKbps = maxVoiceBitrateKbps
	}
	if volume <= 0 {
		volume = DefaultVolume
	}
	return append(args,
		"-i", path,
		"-vn",
		"-ar", "48000",
		"-ac", "2",
		"-af", "volume=" + strconv.FormatFloat(volume, 'f', 3, 64) + ",alimiter=level=disabled:limit=0.891",
		"-c:a", "libopus",
		"-application", "audio",
		"-b:a", strconv.Itoa(bitrateKbps)+"k",
		"-frame_duration", strconv.Itoa(frameMs),
		// A page per tenth of a second instead of ffmpeg's default second: a
		// fresh run hands over its first audio packets immediately rather than
		// after a page fills, which is what a transition is waiting for.
		"-page_duration", "100000",
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
