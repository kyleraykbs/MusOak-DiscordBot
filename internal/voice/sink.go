// Package voice plays a musoak room's song into a Discord voice channel. It
// is one of the Sink implementations pkg/client anticipates: ffmpeg turns the
// cached Ogg/Opus file into fixed 20ms frames on stdout, an Ogg page parser
// splits them into packets, and discordgo's voice connection paces the packets
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

	"codeberg.org/kyleraykbs/musoak/pkg/client"
)

// Sender is the voice connection as the sink needs it: hand a packet over,
// say whether this member is speaking. DiscordSender adapts discordgo's
// VoiceConnection; tests use a fake.
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
	proc    *runProc
	cancel  context.CancelFunc // kills the current ffmpeg
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
	s.seekTo = positionMs
	s.seekReq = true
	rp, active := s.proc, s.active
	if !active {
		// Nothing streams: adopt the position for whoever plays next.
		s.posMs = positionMs
		s.sent = 0
		s.seekReq = false
	}
	s.mu.Unlock()

	if active && rp != nil {
		// Wakes a reader blocked mid-file; the loop then sees the pending
		// seek. A parked loop finds the request when it resumes instead.
		rp.kill()
	}
	return nil
}

// PositionMs is where the audible audio is: the run's start plus the frames
// pushed out. It freezes during a pause because no frames move.
func (s *Sink) PositionMs(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.posMs + s.sent*frameMs, nil
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
		}

		if err := s.waitWhilePaused(ctx); err != nil {
			s.abort()
			return err
		}
		if s.seekPending() {
			continue // parked or raced: go relaunch before reading
		}

		pkt, err := s.read()
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

// abort is the exit for cancel, stop and failure: what is queued is not wanted
// and this member is not speaking.
func (s *Sink) abort() {
	s.sender.Drain()
	_ = s.sender.Speaking(false)
}

// send hands a packet over without wedging forever if the connection died.
func (s *Sink) send(ctx context.Context, pkt []byte) error {
	done := make(chan error, 1)
	go func() {
		done <- s.sender.Send(pkt)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// launch starts ffmpeg at the run's current position, replacing any run that
// is still there.
func (s *Sink) launch(ctx context.Context) error {
	s.mu.Lock()
	path, pos, prev := s.path, s.posMs, s.proc
	s.mu.Unlock()
	if prev != nil {
		prev.kill()
	}

	bin := s.bin
	if resolved, err := exec.LookPath(bin); err == nil {
		bin = resolved
	}
	args := ffmpegArgs(path, pos)
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

func (s *Sink) takeSeek() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.posMs = s.seekTo
	s.sent = 0
	s.seekReq = false
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
