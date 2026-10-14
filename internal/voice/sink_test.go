package voice

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kyleraykbs/MusOak/pkg/client"
)

// fakeSender records what the sink hands over.
type fakeSender struct {
	mu         sync.Mutex
	packets    int
	frames     [][]byte
	speaking   bool
	drainCount int
}

func (f *fakeSender) Send(packet []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.packets++
	f.frames = append(f.frames, append([]byte(nil), packet...))
	return nil
}

func (f *fakeSender) Speaking(on bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.speaking = on
	return nil
}

func (f *fakeSender) Drain() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drainCount++
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.packets
}

func (f *fakeSender) recorded() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.frames...)
}

func (f *fakeSender) drains() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.drainCount
}

func (f *fakeSender) isSpeaking() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.speaking
}

// writeFixturePages writes the packet groups as the pages of ONE Ogg stream
// and returns their paths, so a fake ffmpeg can emit them on a schedule. The
// first page opens with the two headers every Ogg/Opus stream has — the codec
// descriptor and the comment block — because the sink drops a stream's first
// two packets and a fixture without them is not a stream ffmpeg could have
// produced.
func writeFixturePages(t *testing.T, groups [][][]byte) []string {
	t.Helper()
	withHeaders := make([][][]byte, len(groups))
	copy(withHeaders, groups)
	withHeaders[0] = append([][]byte{[]byte("OpusHead"), []byte("OpusTags")}, withHeaders[0]...)
	return writePages(t, withHeaders)
}

// writeFixtureStreams writes each group as a stream of its own — headers and
// all — for tests whose fake ffmpeg is run more than once: every run is a new
// stream, and every stream has its headers.
func writeFixtureStreams(t *testing.T, streams ...[][]byte) []string {
	t.Helper()
	var paths []string
	for i, stream := range streams {
		group := append([][]byte{[]byte("OpusHead"), []byte("OpusTags")}, stream...)
		paths = append(paths, writePages(t, [][][]byte{group})...)
		_ = i
	}
	return paths
}

// writePages writes one file per group, no headers added.
func writePages(t *testing.T, groups [][][]byte) []string {
	t.Helper()
	dir := t.TempDir()
	var paths []string
	for i, group := range groups {
		path := filepath.Join(dir, "page"+string(rune('0'+i))+".ogg")
		if err := os.WriteFile(path, buildPage(group, i > 0 && false), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	return paths
}

// writeScript writes an executable stand-in for ffmpeg that follows the
// given commands ("cat <file>", "sleep <seconds>").
func writeScript(t *testing.T, commands ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-ffmpeg")
	body := "#!/bin/sh\n" + strings.Join(commands, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func sinkFor(t *testing.T, script string) (*Sink, *fakeSender) {
	t.Helper()
	sender := &fakeSender{}
	return NewSink(script, sender, nil), sender
}

func TestFFmpegArgsVolume(t *testing.T) {
	// Music is mastered hot: the default takes a quarter off, and the config
	// can say otherwise.
	if args := ffmpegArgs("/f", 0, 0, 0); !strings.Contains(strings.Join(args, " "), "-af volume=0.750") {
		t.Errorf("default volume missing: %v", args)
	}
	if args := ffmpegArgs("/f", 0, 0, 0.4); !strings.Contains(strings.Join(args, " "), "-af volume=0.400") {
		t.Errorf("configured volume missing: %v", args)
	}
}

func TestFFmpegArgsBitrateFollowsTheChannel(t *testing.T) {
	// The channel's own bitrate is what Discord caps the audio at, so it is
	// what the encoder targets; zero falls back to the default, and Discord's
	// ceiling is never exceeded.
	cases := []struct {
		kbps int
		want string
	}{
		{0, "-b:a 192k"},
		{128, "-b:a 128k"},
		{256, "-b:a 256k"},
		{512, "-b:a 256k"},
	}
	for _, tc := range cases {
		args := ffmpegArgs("/cache/x.opus", 0, tc.kbps, 0)
		if joined := strings.Join(args, " "); !strings.Contains(joined, tc.want) {
			t.Errorf("bitrate %d: %s, want %s", tc.kbps, joined, tc.want)
		}
	}
}

func TestFFmpegArgsSeekAndFormat(t *testing.T) {
	args := ffmpegArgs("/cache/x.opus", 91_500, 0, 0)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-ss 91.500") {
		t.Errorf("no input seek at the position: %s", joined)
	}
	// -ss must be an input option, before -i.
	ss, in := indexOf(args, "-ss"), indexOf(args, "-i")
	if ss < 0 || in < 0 || ss > in {
		t.Errorf("-ss must come before -i: %s", joined)
	}
	for _, want := range []string{"-ar 48000", "-ac 2", "-frame_duration 20", "-f opus", "pipe:1", "-application audio", "-page_duration 100000"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
	if strings.Contains(joined, "-ss") && strings.Contains(joined, "0.000") {
		// Position zero is not worth a seek flag.
		t.Errorf("zero position should not add -ss: %s", joined)
	}
	if args := ffmpegArgs("p", 0, 0, 0); indexOf(args, "-ss") != -1 {
		t.Errorf("position 0 produced a seek: %v", args)
	}
}

func indexOf(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}

// reportingSender is a fake that counts pulls the way a real connection does:
// monotonically, across runs.
type reportingSender struct {
	fakeSender

	mu     sync.Mutex
	played int64
}

func (r *reportingSender) Send(packet []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.packets++
	return nil
}

func (r *reportingSender) PlayedFrames() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.played
}

func (r *reportingSender) pull(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.played += int64(n)
}

// A sender that counts pulls counts them for the whole connection, and a
// position belongs to one run: a second track must not inherit the first
// track's frames as if they were its own, which is a position minutes ahead of
// the room and a seek every beat.
func TestPositionIsPerRunNotPerConnection(t *testing.T) {
	pages := writeFixturePages(t, [][][]byte{{[]byte("one"), []byte("two")}})
	script := writeScript(t, "cat "+pages[0])
	sender := &reportingSender{}
	sink := NewSink(script, sender, nil)

	if err := sink.Play(context.Background(), client.SinkTrack{Path: "a.opus", PositionMs: 1_000}); err != nil {
		t.Fatalf("first Play: %v", err)
	}
	sender.pull(2)
	pos, _ := sink.PositionMs(context.Background())
	if want := int64(1_000 + 2*frameMs); pos != want {
		t.Fatalf("first run position = %d, want %d", pos, want)
	}

	// The connection has played plenty; the next run starts where it is told.
	if err := sink.Play(context.Background(), client.SinkTrack{Path: "b.opus", PositionMs: 30_000}); err != nil {
		t.Fatalf("second Play: %v", err)
	}
	pos, _ = sink.PositionMs(context.Background())
	if pos != 30_000 {
		t.Fatalf("second run position = %d, want 30000 (the first run's frames are not its own)", pos)
	}
}

func TestPlayStreamsTheWholeFileAndReportsPosition(t *testing.T) {
	pages := writeFixturePages(t, [][][]byte{
		{[]byte("one"), []byte("two"), []byte("three")},
	})
	script := writeScript(t, "cat "+pages[0])
	sink, sender := sinkFor(t, script)

	err := sink.Play(context.Background(), client.SinkTrack{Path: "ignored.opus", PositionMs: 1_000})
	if err != nil {
		t.Fatalf("Play: %v", err)
	}
	if sender.count() != 3 {
		t.Errorf("packets sent = %d, want 3", sender.count())
	}
	pos, _ := sink.PositionMs(context.Background())
	if want := int64(1_000 + 3*frameMs); pos != want {
		t.Errorf("position = %d, want %d", pos, want)
	}
	if !sender.isSpeaking() {
		t.Error("the sink stopped speaking on a clean end")
	}
}

func TestPauseFreezesThePositionAndResumeCarriesOn(t *testing.T) {
	pages := writeFixturePages(t, [][][]byte{
		{[]byte("first")},
		{[]byte("second"), []byte("third")},
	})
	script := writeScript(t, "cat "+pages[0], "sleep 1", "cat "+pages[1])
	sink, sender := sinkFor(t, script)

	done := make(chan error, 1)
	go func() {
		done <- sink.Play(context.Background(), client.SinkTrack{Path: "ignored.opus", PositionMs: 500})
	}()

	waitFor(t, 5*time.Second, "the first packet", func() bool { return sender.count() >= 1 })
	if err := sink.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	pausedAt, _ := sink.PositionMs(context.Background())

	// The fake ffmpeg writes page two during its sleep; nothing may be sent
	// and the position may not move while parked.
	time.Sleep(400 * time.Millisecond)
	if got := sender.count(); got != 1 {
		t.Errorf("packets sent while paused = %d, want 1", got)
	}
	if pos, _ := sink.PositionMs(context.Background()); pos != pausedAt {
		t.Errorf("position moved while paused: %d -> %d", pausedAt, pos)
	}

	if err := sink.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Play after resume: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the file never finished after resume")
	}
	if sender.count() != 3 {
		t.Errorf("packets sent = %d, want 3", sender.count())
	}
}

func TestSeekRelaunchesAtTheNewPosition(t *testing.T) {
	pages := writeFixturePages(t, [][][]byte{
		{[]byte("first")},
		{[]byte("second"), []byte("third")},
	})
	script := writeScript(t, "cat "+pages[0], "sleep 2", "cat "+pages[1])
	sink, sender := sinkFor(t, script)

	done := make(chan error, 1)
	go func() {
		done <- sink.Play(context.Background(), client.SinkTrack{Path: "ignored.opus", PositionMs: 0})
	}()

	waitFor(t, 5*time.Second, "the first packet", func() bool { return sender.count() >= 1 })
	if err := sink.Seek(context.Background(), 5_000); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "the play to finish", func() bool {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Play after seek: %v", err)
			}
			return true
		default:
			return false
		}
	})

	// The relaunch starts at 5s; the full second run then plays out.
	pos, _ := sink.PositionMs(context.Background())
	if want := int64(5_000 + 3*frameMs); pos != want {
		t.Errorf("position after seek = %d, want %d", pos, want)
	}
}

func TestStopEndsPlaybackPromptly(t *testing.T) {
	pages := writeFixturePages(t, [][][]byte{{[]byte("first")}})
	script := writeScript(t, "cat "+pages[0], "sleep 30")
	sink, sender := sinkFor(t, script)

	done := make(chan error, 1)
	go func() {
		done <- sink.Play(context.Background(), client.SinkTrack{Path: "ignored.opus", PositionMs: 0})
	}()
	waitFor(t, 5*time.Second, "the first packet", func() bool { return sender.count() >= 1 })

	started := time.Now()
	if err := sink.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Play did not return after Stop")
	}
	if took := time.Since(started); took > 5*time.Second {
		t.Errorf("Stop took %s; the child's sleep is holding the pipe", took)
	}
	if sender.isSpeaking() {
		t.Error("still speaking after Stop")
	}
}

// A track change must not throw away what is already queued: that tail is the
// song that just ended, and it is what covers ffmpeg starting the next one.
// A seek is the opposite — the frames queued are from the position being left.
// A stream's first two packets are OpusHead and OpusTags: sending them as
// frames puts two junk packets into the room at the start of every song.
func TestTheStreamHeadersAreNotSentAsAudio(t *testing.T) {
	pages := writeFixturePages(t, [][][]byte{{[]byte("first-audio"), []byte("second-audio")}})
	script := writeScript(t, "cat "+pages[0])
	sink, sender := sinkFor(t, script)

	if err := sink.Play(context.Background(), client.SinkTrack{Path: "a.opus", PositionMs: 0}); err != nil {
		t.Fatalf("Play: %v", err)
	}
	got := sender.recorded()
	if len(got) != 2 {
		t.Fatalf("frames sent = %d, want the two audio packets and no headers", len(got))
	}
	if string(got[0]) != "first-audio" || string(got[1]) != "second-audio" {
		t.Errorf("frames = %q, %q", got[0], got[1])
	}
}

func TestATrackChangeKeepsTheTailAndASeekDropsIt(t *testing.T) {
	streams := writeFixtureStreams(t,
		[][]byte{[]byte("a"), []byte("b")},
		[][]byte{[]byte("c"), []byte("d"), []byte("e")},
	)
	slow := writeScript(t, "cat "+streams[0], "sleep 30")
	fast := writeScript(t, "cat "+streams[1])

	sink, sender := sinkFor(t, slow)
	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := make(chan error, 1)
	go func() { done1 <- sink.Play(ctx1, client.SinkTrack{Path: "one.opus", PositionMs: 0}) }()
	waitFor(t, 5*time.Second, "the first track", func() bool { return sender.count() >= 1 })

	// The room moves on: the old run is cancelled, and what it queued stays.
	cancel1()
	<-done1
	if drained := sender.drains(); drained != 0 {
		t.Errorf("the track change drained the buffer (%d drains)", drained)
	}

	// A seek, on the other hand, must drop frames read from the old position.
	before := sender.drains()
	sink.bin = fast
	done2 := make(chan error, 1)
	go func() { done2 <- sink.Play(context.Background(), client.SinkTrack{Path: "two.opus", PositionMs: 0}) }()
	waitFor(t, 5*time.Second, "the second track", func() bool { return sender.count() >= 3 })
	if err := sink.Seek(context.Background(), 5_000); err != nil {
		t.Fatal(err)
	}
	if sender.drains() <= before {
		t.Error("a seek did not drop the frames read from the old position")
	}
	<-done2
}

func TestANewTrackWaitsForTheOldRunToLeave(t *testing.T) {
	streams := writeFixtureStreams(t,
		[][]byte{[]byte("a"), []byte("b")},
		[][]byte{[]byte("c"), []byte("d"), []byte("e")},
	)
	slow := writeScript(t, "cat "+streams[0], "sleep 30")
	fast := writeScript(t, "cat "+streams[1])

	sink, sender := sinkFor(t, slow)
	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := make(chan error, 1)
	go func() { done1 <- sink.Play(ctx1, client.SinkTrack{Path: "one.opus", PositionMs: 0}) }()
	waitFor(t, 5*time.Second, "the first track", func() bool { return sender.count() >= 1 })

	cancel1()
	// The second Play must block until the first loop has actually left, then
	// run a clean stream of its own.
	done2 := make(chan error, 1)
	go func() {
		sink.bin = fast
		done2 <- sink.Play(context.Background(), client.SinkTrack{Path: "two.opus", PositionMs: 7_000})
	}()

	select {
	case err := <-done1:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("first Play = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first Play never returned after its context was cancelled")
	}
	select {
	case err := <-done2:
		if err != nil {
			t.Fatalf("second Play: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second Play never ran")
	}
	// Two packets of the first run leaked to the sender before the cancel;
	// the second run's accounting must be its own.
	pos, _ := sink.PositionMs(context.Background())
	if want := int64(7_000 + 3*frameMs); pos != want {
		t.Errorf("second run position = %d, want %d (its own accounting)", pos, want)
	}
}

func TestWithRealFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not on PATH")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "sine.ogg")
	gen := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-c:a", "libopus", src)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("generate fixture: %v: %s", err, out)
	}

	sink, sender := sinkFor(t, "ffmpeg")
	if err := sink.Play(context.Background(), client.SinkTrack{Path: src, PositionMs: 0}); err != nil {
		t.Fatalf("Play: %v", err)
	}
	// One second of audio in 20ms frames.
	if n := sender.count(); n < 45 || n > 55 {
		t.Errorf("packets for a one-second file = %d, want ~50", n)
	}
	pos, _ := sink.PositionMs(context.Background())
	if pos < 800 || pos > 1_200 {
		t.Errorf("position = %d, want about 1000", pos)
	}
}
