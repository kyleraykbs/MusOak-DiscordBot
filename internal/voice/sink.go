// Package voice plays a musoak room's song into a Discord voice channel. It
// is one of the Sink implementations pkg/client anticipates: ffmpeg turns the
// cached Ogg/Opus file into fixed 20ms frames on stdout, an Ogg page parser
// splits them into packets, and disgo's voice connection paces the packets
// onto the wire at 20ms each.
package voice

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/kyleraykbs/MusOak/pkg/client"
)

// PlayedReporter is a Sender that knows how many frames have actually left for
// the wire; the sink prefers it for position.
type PlayedReporter interface {
	PlayedFrames() int64
}

// Sender is the voice connection as the sink needs it: hand a packet over,
// say whether this member is speaking. ConnSender adapts disgo's voice
// connection; tests use a fake.
type Sender interface {
	// Send hands one opus packet to the connection. It may block while the
	// connection drains, but not forever.
	Send(packet []byte) error
	// Speaking sets the voice state.
	Speaking(on bool) error
	// Drain drops whatever packets are still queued: the audio behind them is
	// no longer wanted (a pause, a stop, an abandoned track).
	Drain()
}

// Sink plays one file into a voice connection, honoring the whole pkg/client
// Sink contract: Play runs to the end of the file (the participant advances on
// its own return), pause parks the stream in place, seek relaunches ffmpeg at
// the new position, and PositionMs reports where the audible audio is.
//
// One Play runs at a time: a new Play waits for the previous loop to leave,
// which the participant's cancel-before-start ordering makes brief.
type Sink struct {
	bin    string
	sender Sender
	logger *slog.Logger

	mu      sync.Mutex
	turn    *sync.Cond // the handoff between Plays
	active  bool       // a Play loop holds the slot
	pauseCh chan struct{}
	path    string
	posMs   int64 // position of the first packet of the current run
	sent    int64 // packets pushed in the current run
	seekReq bool
	seekTo  int64
	// bitrateKbps is what the encoder targets: the voice channel's own, which
	// is where Discord caps it. Zero keeps the default.
	bitrateKbps int
	// volume is applied before encoding; zero keeps the default.
	volume float64
	// headerPackets counts the Ogg headers already dropped for this run: the
	// first two packets of a stream are OpusHead and OpusTags, not audio.
	headerPackets int
	// playedBase is the sender's pull count when this run began. The sender
	// counts for the whole connection; a position belongs to one run, so the
	// difference is what this run has played.
	playedBase int64
	proc       *runProc
	cancel     context.CancelFunc // kills the current ffmpeg
}

// NewSink builds a sink that launches bin as ffmpeg ("ffmpeg" when empty).
func NewSink(bin string, sender Sender, logger *slog.Logger) *Sink {
	if bin == "" {
		bin = "ffmpeg"
	}
	if logger == nil {
		logger = slog.Default()
	}
	s := &Sink{bin: bin, sender: sender, logger: logger}
	s.turn = sync.NewCond(&s.mu)
	return s
}

var (
	_ client.Sink       = (*Sink)(nil)
	_ client.Pauser     = (*Sink)(nil)
	_ client.Seeker     = (*Sink)(nil)
	_ client.Positioner = (*Sink)(nil)
)

// runProc is one ffmpeg run: its reader, its captured stderr, and a single
// kill/reap no matter how many paths decide the run is over. The kill takes
// the whole process group, because a writer the launch left behind (a helper,
// a wrapper script) would otherwise keep stdout open and stall the reader.
type runProc struct {
	cmd    *exec.Cmd
	reader *oggReader
	stderr *bytes.Buffer
	once   sync.Once
}

func (r *runProc) kill() {
	r.once.Do(func() {
		if r.cmd.Process != nil {
			_ = syscall.Kill(-r.cmd.Process.Pid, syscall.SIGKILL)
			_ = r.cmd.Process.Kill()
		}
		_ = r.cmd.Wait()
	})
}

// Play streams track.Path from track.PositionMs until the file ends, ctx is
// cancelled, or the sink is stopped. A nil return means the file ran out and
// the room may move on; a cancelled ctx means somebody replaced this track.
func (s *Sink) Play(ctx context.Context, track client.SinkTrack) error {
	s.mu.Lock()
	for s.active {
		s.turn.Wait()
	}
	s.active = true
	s.path = track.Path
	s.posMs = track.PositionMs
	if s.posMs < 0 {
		s.posMs = 0
	}
	s.sent = 0
	s.seekReq = false
	if reporter, ok := s.sender.(PlayedReporter); ok {
		s.playedBase = reporter.PlayedFrames()
	}
	s.closePauseLocked()
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.mu.Unlock()

	err := s.stream(runCtx)

	cancel()
	s.killProc()
	s.mu.Lock()
	s.active = false
	s.cancel = nil
	s.turn.Broadcast()
	s.mu.Unlock()

	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		s.logger.Debug("voice: playback ended with an error", "error", err)
	}
	return err
}

// SetBitrate tells the sink how many kbps the voice channel allows, which is
// the ceiling Discord enforces. Zero keeps the default; the value applies to
// the next track.
func (s *Sink) SetBitrate(kbps int) {
	s.mu.Lock()
	s.bitrateKbps = kbps
	s.mu.Unlock()
}

// SetVolume sets what the encoder is fed, as a fraction of the file's own
// level. Zero keeps the default.
func (s *Sink) SetVolume(volume float64) {
	s.mu.Lock()
	s.volume = volume
	s.mu.Unlock()
}

// Stop abandons whatever is playing: no more packets, not speaking. The Play
// loop is cancelled and reaped; if it does not leave within three seconds the
// caller goes on without it (it is already unreachable).
func (s *Sink) Stop(ctx context.Context) error {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	s.closePauseLocked()
	s.mu.Unlock()

	s.killProc()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		active := s.active
		s.mu.Unlock()
		if !active {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	_ = s.sender.Speaking(false)
	s.sender.Drain()
	return nil
}

// Pause parks the stream: ffmpeg keeps its place in the file (blocked on the
// pipe), packets stop flowing, and PositionMs freezes where the audio was.
func (s *Sink) Pause(ctx context.Context) error {
	s.mu.Lock()
	if s.pauseCh == nil {
		s.pauseCh = make(chan struct{})
	}
	s.mu.Unlock()
	_ = s.sender.Speaking(false)
	s.sender.Drain()
	return nil
}

// Resume continues the parked stream in place.
func (s *Sink) Resume(ctx context.Context) error {
	s.mu.Lock()
	s.closePauseLocked()
	s.mu.Unlock()
	_ = s.sender.Speaking(true)
	return nil
}

// Seek moves the audible position. The running ffmpeg is killed (or, when
// parked, replaced on resume) and relaunched at positionMs by the play loop.
func (s *Sink) Seek(ctx context.Context, positionMs int64) error {
	if positionMs < 0 {
		positionMs = 0
	}
	s.mu.Lock()
	if s.active && samePacket(s.positionLocked(), positionMs) {
		// Already there. A kill, a relaunch, a drain and the refill behind
		// them is a hiccup heard for a position the listener is not leaving.
		s.mu.Unlock()
		return nil
	}
	s.seekTo = positionMs
	s.seekReq = true
	rp, active := s.proc, s.active
	if !active {
		// Nothing streams: adopt the position for whoever plays next, and drop
		// what is queued here. There is no play loop to ask, and those frames
		// were read from the position being left, so they are stale either way.
		s.posMs = positionMs
		s.sent = 0
		s.seekReq = false
		s.mu.Unlock()
		s.sender.Drain()
		return nil
	}
	s.mu.Unlock()

	if active && rp != nil {
		// Wakes a reader blocked mid-file; the loop then sees the pending
		// seek, warms the new run and only then drops what is queued. The drop
		// is deliberately not done here: the frames still queued are what plays
		// while ffmpeg restarts, so dropping them the moment a seek is asked
		// for is the silence right after it.
		rp.kill()
	}
	return nil
}

// PositionMs is where the audible audio is: the run's start plus the frames
// that have left for the wire. A sender that can say how many those are is
// asked, because a buffer ahead of the wire is not audible yet — counting what
// was pushed would report the buffer as played and pull the room's drift
// correction backwards. A sender that cannot say falls back to what it was
// given, which is exact only while the buffer is empty.
//
// The run's start is posMs and its frames start at playedBase; a seek moves
// both, which is what keeps the sum describing the new position rather than
// the one being left.
func (s *Sink) PositionMs(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.positionLocked(), nil
}

// positionLocked is PositionMs with the lock already held.
func (s *Sink) positionLocked() int64 {
	if reporter, ok := s.sender.(PlayedReporter); ok {
		played := reporter.PlayedFrames() - s.playedBase
		if played < 0 {
			played = 0
		}
		return s.posMs + played*frameMs
	}
	return s.posMs + s.sent*frameMs
}

// stream is the Play loop. Invariants worth keeping in mind while reading:
// only this goroutine launches or reads a run; Seek and Stop only signal (a
// flag, a kill, a cancelled context) and the loop reacts at its checkpoints.
func (s *Sink) stream(ctx context.Context) error {
	if err := s.launch(ctx); err != nil {
		return err
	}
	if err := s.sender.Speaking(true); err != nil {
		s.logger.Debug("voice: speaking", "error", err)
	}
	// The start of a track waits for the same thing a seek does: audio in hand,
	// read before anything is handed to the wire - a stability buffer's worth of
	// it, so the connection starts three seconds ahead and stays there.
	if err := s.sendAll(ctx, s.fill(stabilityPackets)); err != nil {
		s.abort()
		return err
	}

	for {
		if err := ctx.Err(); err != nil {
			s.abort()
			return err
		}

		if s.seekPending() {
			s.takeSeek()
			if err := s.launch(ctx); err != nil {
				s.abort()
				return err
			}
			// The new position reaches the wire only once its run has a
			// stability buffer in hand; until then the frames queued from the
			// position being left are still playing, which is what covers the
			// relaunch. Dropping them when the seek was asked for is what left
			// the connection silent until ffmpeg was ready.
			lead := s.fill(stabilityPackets)
			if s.seekPending() {
				continue // a newer seek arrived while this run was warming
			}
			s.sender.Drain()
			// The drain emptied the queue, so every frame the sender counts
			// from here on belongs to this run: rebase before the lead goes
			// out, or the frames of the position being left stay in the
			// count and the position reads a whole run ahead of the audio.
			s.rebasePosition()
			if err := s.sendAll(ctx, lead); err != nil {
				s.abort()
				return err
			}
		}

		if err := s.waitWhilePaused(ctx); err != nil {
			s.abort()
			return err
		}
		if s.seekPending() {
			continue // parked or raced: go relaunch before reading
		}

		pkt, err := s.read()
		if err == nil && s.dropHeaderPacket() {
			continue // OpusHead or OpusTags: a header is not a frame
		}
		if err != nil {
			if s.seekPending() {
				continue
			}
			if ctx.Err() != nil {
				s.abort()
				return ctx.Err()
			}
			if errors.Is(err, io.EOF) {
				// The file ran out: the tail still queued at the connection
				// plays out on its own, so no drain here.
				return nil
			}
			s.abort()
			return err
		}

		if err := s.waitWhilePaused(ctx); err != nil {
			s.abort()
			return err
		}
		if s.seekPending() {
			continue // the packet predates the seek; drop it
		}

		if err := s.send(ctx, pkt); err != nil {
			s.abort()
			return err
		}
		s.countPacket()
	}
}

// abort is the exit for cancel, stop and failure. What is queued stays: on a
// track change it is the tail of the song that just ended, and playing it out
// is what covers the relaunch of ffmpeg for the next one — draining it there
// was the gap heard between songs. Pause, seek and stop drop their stale
// frames themselves.
func (s *Sink) abort() {
	_ = s.sender.Speaking(false)
}

// stabilityPackets is how much audio the sink tries to have queued ahead of the
// wire: three seconds of 20ms packets.
//
// This is the buffer a poor connection needs. The wire is fed in bursts and it
// stalls - a congested uplink, a lost packet, a round trip that took the long
// way - and whatever is already queued is what plays through the stall. Without
// it the connection runs dry mid-song, and running dry is the clicking. Three
// seconds is the delay this had before it was reduced to a single packet's
// worth of warm-up; on a bad line the delay is the point.
const stabilityPackets = 150

// fill reads up to n audio packets, taking only what the producer has already
// written: the reader's own buffer is what it goes by, so a run that is
// producing slowly hands over what it has rather than holding up the start of a
// song waiting for a buffer that is not coming. The first packet is read
// blocking, because a run that has produced nothing yet is a run that is not
// ready - and that wait is covered by whatever the connection is still playing.
func (s *Sink) fill(n int) [][]byte {
	out := make([][]byte, 0, n)
	for len(out) < n {
		if len(out) > 0 && !s.readerReady() {
			break // nothing more on hand: the loop picks it up as it arrives
		}
		pkt, err := s.read()
		if err != nil {
			break
		}
		if s.dropHeaderPacket() {
			continue // OpusHead or OpusTags: a header is not a frame
		}
		out = append(out, pkt)
	}
	return out
}

// readerReady is whether the current run has more in hand right now.
func (s *Sink) readerReady() bool {
	s.mu.Lock()
	rp := s.proc
	s.mu.Unlock()
	return rp != nil && rp.reader.ready()
}

// sendAll hands over the packets a fill collected, the same way the loop sends
// the ones it reads one at a time.
func (s *Sink) sendAll(ctx context.Context, packets [][]byte) error {
	for _, pkt := range packets {
		if err := s.send(ctx, pkt); err != nil {
			return err
		}
		s.countPacket()
	}
	return nil
}

// send hands a packet over. The sender blocks until it has room, which is the
// backpressure that keeps the buffer honest, and unblocks on its own if the
// connection goes away — so this needs no goroutine per frame.
func (s *Sink) send(ctx context.Context, pkt []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.sender.Send(pkt)
}

// launch starts ffmpeg at the run's current position, replacing any run that
// is still there.
func (s *Sink) launch(ctx context.Context) error {
	s.mu.Lock()
	path, pos, bitrate, volume, prev := s.path, s.posMs, s.bitrateKbps, s.volume, s.proc
	s.headerPackets = 0
	s.mu.Unlock()
	if prev != nil {
		prev.kill()
	}

	bin := s.bin
	if resolved, err := exec.LookPath(bin); err == nil {
		bin = resolved
	}
	args := ffmpegArgs(path, pos, bitrate, volume)
	cmd := exec.CommandContext(ctx, bin, args...)
	// Own process group: a cancelled ctx (or a seek's kill) takes every
	// process the launch made, so the pipe closes and the reader wakes.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("voice: ffmpeg pipe: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	s.logger.Debug("voice: launching", "command", command(bin, args))
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("voice: start ffmpeg: %w", err)
	}

	s.mu.Lock()
	s.proc = &runProc{cmd: cmd, reader: newOggReader(stdout), stderr: &stderr}
	s.mu.Unlock()
	return nil
}

// read pulls the next packet, reaping the run when its stream ends.
func (s *Sink) read() ([]byte, error) {
	s.mu.Lock()
	rp := s.proc
	s.mu.Unlock()
	if rp == nil {
		return nil, io.EOF
	}
	pkt, err := rp.reader.next()
	if err != nil {
		rp.kill() // EOF waits for the exit status; a seek-kill is a no-op
		if errors.Is(err, io.EOF) && rp.cmd.ProcessState != nil && rp.cmd.ProcessState.ExitCode() != 0 {
			return nil, fmt.Errorf("voice: ffmpeg exited %d: %s",
				rp.cmd.ProcessState.ExitCode(), trimOutput(rp.stderr.String()))
		}
		return nil, err
	}
	return pkt, nil
}

// dropHeaderPacket reports whether this packet is one of the stream's two
// headers, which must never be sent as audio: they are a codec descriptor and
// a comment block, and a decoder handed either produces noise.
func (s *Sink) dropHeaderPacket() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.headerPackets >= 2 {
		return false
	}
	s.headerPackets++
	return true
}

// waitWhilePaused blocks while the stream is parked.
func (s *Sink) waitWhilePaused(ctx context.Context) error {
	s.mu.Lock()
	ch := s.pauseCh
	s.mu.Unlock()
	if ch == nil {
		return nil
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Sink) seekPending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seekReq
}

// takeSeek adopts the pending seek's position for the run about to start.
func (s *Sink) takeSeek() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.posMs = s.seekTo
	s.sent = 0
	s.seekReq = false
}

// rebasePosition restarts this run's frame count at the sender's current one.
//
// A sender counts pulls for the whole connection, and a position belongs to one
// run from one start. A seek's drain is the moment the queue holds nothing from
// the position being left, so every frame still to come belongs to the run
// about to be handed over, and counting from there is what keeps PositionMs
// describing the audio. Without it the frames played before the seek stay in
// the difference: the position reads a whole run-time ahead of where the audio
// is, and whoever compares it with the room's own position — the follower's
// drift correction — sees a disagreement too large to be one and stops
// correcting until the track changes.
func (s *Sink) rebasePosition() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if reporter, ok := s.sender.(PlayedReporter); ok {
		s.playedBase = reporter.PlayedFrames()
	}
}

// samePacket reports whether two positions are the same 20ms frame of audio.
func samePacket(a, b int64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < frameMs
}

func (s *Sink) countPacket() {
	s.mu.Lock()
	s.sent++
	s.mu.Unlock()
}

func (s *Sink) closePauseLocked() {
	if s.pauseCh != nil {
		close(s.pauseCh)
		s.pauseCh = nil
	}
}

func (s *Sink) killProc() {
	s.mu.Lock()
	rp := s.proc
	s.proc = nil
	s.mu.Unlock()
	if rp != nil {
		rp.kill()
	}
}

// trimOutput keeps a failed ffmpeg's complaint for the log without letting a
// chatty failure fill memory.
func trimOutput(out string) string {
	const max = 4 << 10
	out = trimSpace(out)
	if len(out) > max {
		return out[:max] + "…"
	}
	return out
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\n' || s[start] == '\t' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\n' || s[end-1] == '\t' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}
