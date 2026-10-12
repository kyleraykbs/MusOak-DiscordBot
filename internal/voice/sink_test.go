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

	"codeberg.org/kyleraykbs/musoak/pkg/client"
)

// fakeSender records what the sink hands over.
type fakeSender struct {
	mu       sync.Mutex
	packets  int
	speaking bool
	drains   int
}

func (f *fakeSender) Send(packet []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.packets++
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
	f.drains++
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.packets
}

func (f *fakeSender) isSpeaking() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.speaking
}

// writeFixturePages writes each packet group as its own Ogg page file and
// returns their paths, so a fake ffmpeg can emit them on a schedule.
func writeFixturePages(t *testing.T, groups [][][]byte) []string {
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
		args := ffmpegArgs("/cache/x.opus", 0, tc.kbps)
		if joined := strings.Join(args, " "); !strings.Contains(joined, tc.want) {
			t.Errorf("bitrate %d: %s, want %s", tc.kbps, joined, tc.want)
		}
	}
}

func TestFFmpegArgsSeekAndFormat(t *testing.T) {
	args := ffmpegArgs("/cache/x.opus", 91_500, 0)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-ss 91.500") {
		t.Errorf("no input seek at the position: %s", joined)
	}
	// -ss must be an input option, before -i.
	ss, in := indexOf(args, "-ss"), indexOf(args, "-i")
	if ss < 0 || in < 0 || ss > in {
		t.Errorf("-ss must come before -i: %s", joined)
	}
	for _, want := range []string{"-ar 48000", "-ac 2", "-frame_duration 20", "-f opus", "pipe:1", "-application audio"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
	if strings.Contains(joined, "-ss") && strings.Contains(joined, "0.000") {
		// Position zero is not worth a seek flag.
		t.Errorf("zero position should not add -ss: %s", joined)
	}
	if args := ffmpegArgs("p", 0, 0); indexOf(args, "-ss") != -1 {
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

func TestANewTrackWaitsForTheOldRunToLeave(t *testing.T) {
	pages := writeFixturePages(t, [][][]byte{
		{[]byte("a"), []byte("b")},
		{[]byte("c"), []byte("d"), []byte("e")},
	})
	slow := writeScript(t, "cat "+pages[0], "sleep 30")
	fast := writeScript(t, "cat "+pages[1])

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
