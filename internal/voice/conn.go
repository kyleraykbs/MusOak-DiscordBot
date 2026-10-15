package voice

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/disgoorg/disgo/voice"
)

// ConnSender is Sender for a disgo voice connection. Frames hand off through a
// queue that the connection's own audio sender pulls from once every 20ms; the
// connection wraps them in DAVE and transport encryption on the way out, which
// is the part Discord's v8 voice gateway requires and discordgo's own sender
// never did.
//
// The queue is the buffer. It holds `bufferFrames` of audio ahead of the wire
// so a hiccup — an ffmpeg relaunch between songs, a scheduling blip — is
// absorbed instead of heard: the pull loop treats an empty queue as silence,
// and enough of that is a drop.
//
// The connection's sender is not immortal: it closes itself for good when a
// send fails on a closed socket or a voice gateway that went away, and then
// nothing pulls and the audio stops while the room runs on. A watchdog notices
// the silence and re-installs the provider, which is the connection's own way
// to start a fresh pull loop.
type ConnSender struct {
	conn   voice.Conn
	queue  chan []byte
	logger *slog.Logger
	// hold reports whether the DAVE group is mid-change, in which case frames
	// are kept in the queue rather than sent: a listener that has moved to the
	// new epoch cannot decrypt the old one, so sending them is a dropout with
	// extra steps. Nil means never hold.
	hold func() bool

	// done is closed when the connection will not take audio at all; Send then
	// fails, which is what stops a sink from waiting on it forever.
	done chan struct{}
	once sync.Once

	played atomic.Int64 // frames the connection has actually pulled
	// drainedAt is when the queue was last emptied on purpose, so a dry run
	// can be attributed to it (unix millis; zero until the first drain).
	drainedAt atomic.Int64
	// dry counts the pulls that found nothing queued since the last one that
	// did: a run of them is a gap in the audio, and it is worth saying out
	// loud rather than leaving to be heard. gaps is how many such runs there
	// have been, and worstFrames the longest of them. They are guarded by
	// statsMu, not mu: a re-install gives the connection a fresh pull loop and
	// closes the old one, and a call the old loop had already started runs
	// beside the new one.
	dry         int
	gaps        int
	worstFrames int
	// holding is whether the last pull held, so the transition is logged once.
	holding    bool
	heldFrames int64
	statsMu    sync.Mutex

	// The watchdog's knobs, so a test can make it quick.
	watchEvery  time.Duration
	giveUpAfter int
	// reconnectTimeout bounds one rebuild, and rebuildLimit how many this
	// sender may make before it gives up on the audio.
	reconnectTimeout time.Duration
	rebuildLimit     int

	// reconnect rebuilds the voice connection once a fresh pull loop has not
	// helped. Re-installing a frame provider cannot fix a connection whose own
	// voice socket is gone: disgo's voice conn closes and unregisters itself
	// when its voice gateway drops, which also takes the bot out of the
	// channel, so the frames it was handed go to a closed socket and there is
	// nothing left to re-install into. Nil means the sender can only give up.
	reconnect func(ctx context.Context) (voice.Conn, error)
	// rebuilds counts the connections rebuilt for this sender. Only the
	// watchdog goroutine touches it.
	rebuilds int

	mu       sync.Mutex
	provider *pullProvider // the current install
}

// NewConnSender attaches a frame provider to the connection and returns the
// sender the sink pushes into. bufferFrames is how much audio is kept ahead of
// the wire; anything below a few frames is no buffer at all. reconnect is asked
// to build a fresh connection when this one stops taking audio; nil means the
// sender can only give up on it.
func NewConnSender(conn voice.Conn, bufferFrames int, logger *slog.Logger, hold func() bool, reconnect func(ctx context.Context) (voice.Conn, error)) *ConnSender {
	if bufferFrames < 3 {
		bufferFrames = 3
	}
	if logger == nil {
		logger = slog.Default()
	}
	s := &ConnSender{
		conn:             conn,
		queue:            make(chan []byte, bufferFrames),
		logger:           logger,
		hold:             hold,
		reconnect:        reconnect,
		done:             make(chan struct{}),
		watchEvery:       2 * time.Second,
		giveUpAfter:      5,
		rebuildLimit:     2,
		reconnectTimeout: 15 * time.Second,
	}
	s.install()
	go s.watch()
	return s
}

// install gives the connection a fresh pull loop over this sender's queue. A
// provider is spent once its loop has gone — disgo cancels the loop when it is
// handed another, and lets the provider itself go nowhere — so the install it
// replaces is closed here: a pull it had already started must not take a frame
// out of the queue on its way out.
func (s *ConnSender) install() {
	provider := &pullProvider{sender: s, live: make(chan struct{})}
	s.mu.Lock()
	previous := s.provider
	s.provider = provider
	conn := s.conn
	s.mu.Unlock()
	if previous != nil {
		previous.Close()
	}
	conn.SetOpusFrameProvider(provider)
}

// setConn points the sender at a rebuilt connection and lets the install that
// served the old one go.
func (s *ConnSender) setConn(conn voice.Conn) {
	s.mu.Lock()
	previous := s.provider
	s.conn = conn
	s.mu.Unlock()
	if previous != nil {
		previous.Close()
	}
}

// watch notices a connection that has stopped pulling while frames wait, and
// does something about it.
//
// What it looks at is the rate the frames leave at, not progress itself: a pull
// loop that dies after its first frame still moves the count once per install,
// and treating that as progress is what made this re-install a provider every
// few seconds forever, into a connection that had nothing left to install into.
//
// A connection mid-DAVE-change holds its frames on purpose, and a quiet one has
// no frames to take, so neither counts as a stall.
func (s *ConnSender) watch() {
	last := s.played.Load()
	stalls := 0
	for {
		// The knobs are read per tick rather than once, so a test can shorten
		// the wait after the sender has already started.
		timer := time.NewTimer(s.watchEveryDuration())
		select {
		case <-s.done:
			timer.Stop()
			return
		case <-timer.C:
		}
		played := s.played.Load()
		moved := played - last
		last = played
		if moved >= s.pacedFrames() {
			stalls = 0
			continue
		}
		if len(s.queue) < minQueuedFrames {
			continue // nothing (or almost nothing) waiting: quiet is allowed
		}
		if s.hold != nil && s.hold() {
			continue // the frames are held for a DAVE change, not stuck
		}
		stalls++
		if s.recover(stalls) {
			stalls = 0
			last = s.played.Load()
		}
	}
}

// watchEveryDuration and giveUpAfterStalls read the watchdog's knobs under the
// lock, and tune sets them: a test shortens them after the sender has started,
// which is exactly when the watchdog is reading them.
func (s *ConnSender) watchEveryDuration() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.watchEvery
}

func (s *ConnSender) giveUpAfterStalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.giveUpAfter
}

func (s *ConnSender) tune(every time.Duration, giveUpAfter int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.watchEvery = every
	s.giveUpAfter = giveUpAfter
}

// minQueuedFrames is the backlog that makes a slow tick a stalled connection
// rather than a slow producer: frames actually waiting to be taken. A producer
// that cannot keep up leaves the queue empty, and an empty queue is not this
// connection's fault.
const minQueuedFrames = 5

// pacedFrames is half of what the wire takes in one watch tick, at the one
// frame per frameMs the connection paces at. Half, because a tick can catch the
// queue mid-fill; never less than one, because less than a frame a tick is not
// a connection that is pulling.
func (s *ConnSender) pacedFrames() int64 {
	frames := int64(s.watchEveryDuration() / (frameMs * time.Millisecond))
	if frames < 2 {
		frames = 2
	}
	return frames / 2
}

// recover is the watchdog's answer to a stall, in the order it tries them: a
// fresh pull loop, then a rebuilt connection, and only then giving up on the
// audio, which fails Send and stops a sink waiting on a connection that is
// gone. It reports whether the stall count should start over.
func (s *ConnSender) recover(stalls int) bool {
	if stalls == 1 {
		s.logger.Warn("voice: the connection stopped pulling audio; restarting its sender",
			"queued_frames", len(s.queue), "want_frames_per_tick", s.pacedFrames())
		s.install()
		return false
	}
	if stalls < s.giveUpAfterStalls() {
		return false
	}
	if s.reconnect == nil || s.rebuilds >= s.rebuildLimit {
		s.logger.Error("voice: the voice connection will not take audio; giving up on it",
			"queued_frames", len(s.queue), "rebuilds", s.rebuilds)
		s.close()
		return false
	}
	// The provider is not the fault, the connection is: rebuild it — a fresh
	// voice gateway, a fresh UDP socket, a fresh DAVE session, and a re-join of
	// the voice channel — and follow the audio onto it.
	ctx, cancel := context.WithTimeout(context.Background(), s.reconnectTimeout)
	conn, err := s.reconnect(ctx)
	cancel()
	if err != nil {
		s.logger.Error("voice: could not rebuild the voice connection; giving up on the audio",
			"error", err, "queued_frames", len(s.queue))
		s.close()
		return false
	}
	s.rebuilds++
	s.logger.Warn("voice: the voice connection would not take audio; rebuilt it",
		"queued_frames", len(s.queue), "rebuilds", s.rebuilds)
	s.setConn(conn)
	s.install()
	return true
}

// PlayedFrames is how many frames have left the queue towards the wire, which
// is where the audio is — the position a sink should report.
func (s *ConnSender) PlayedFrames() int64 { return s.played.Load() }

// Send queues one opus packet for the connection's sender. The closed check
// comes first on purpose: a select over a closed channel and a writable one
// picks at random, and a frame handed to a gone connection would be a lie.
func (s *ConnSender) Send(packet []byte) error {
	select {
	case <-s.done:
		return errors.New("voice: the connection is not taking audio")
	default:
	}
	select {
	case <-s.done:
		return errors.New("voice: the connection is not taking audio")
	case s.queue <- packet:
		return nil
	}
}

// Speaking is the connection's own business: its audio sender speaks while
// frames flow and stops after a few frames of silence, so the sink's calls are
// deliberately nothing. Saying it twice would fight the sender for the flag.
func (s *ConnSender) Speaking(on bool) error { return nil }

// Drain drops whatever is queued: the audio behind those frames is no longer
// wanted (a pause, a stop, an abandoned track).
func (s *ConnSender) Drain() {
	// A drain empties the queue on purpose, so a gap that follows one is the
	// sink's own doing (a seek, a pause) rather than the connection starving.
	// The dry warning carries this so the two can be told apart in a log.
	s.drainedAt.Store(time.Now().UnixMilli())
	for {
		select {
		case <-s.queue:
		default:
			return
		}
	}
}

// Close ends the sender for good.
func (s *ConnSender) Close() { s.close() }

func (s *ConnSender) close() {
	s.once.Do(func() { close(s.done) })
}

// provide is one pull: a frame when one is queued, an empty frame when there is
// nothing to play — the connection turns those into a few frames of silence and
// then stops speaking — and EOF once this install is replaced or the sender is
// done, which is how the connection's pull loop is told to let go.
func (s *ConnSender) provide(live chan struct{}) ([]byte, error) {
	select {
	case <-live:
		return nil, io.EOF
	case <-s.done:
		return nil, io.EOF
	default:
	}

	// The queue is not consumed while the DAVE group changes: what is in it
	// stays in it, and the epoch settles in a few frames. Silence goes out
	// instead, which is what the change sounds like either way.
	if s.hold != nil && s.hold() {
		s.noteHeld()
		return nil, nil
	}
	if held, frames := s.takeHolding(); held {
		s.logger.Info("voice: frames flowing again after a DAVE epoch change",
			"held_frames", frames)
	}

	select {
	case frame := <-s.queue:
		s.played.Add(1)
		s.noteDryGap()
		return frame, nil
	case <-live:
		return nil, io.EOF
	case <-s.done:
		return nil, io.EOF
	default:
		s.countDry()
		return nil, nil
	}
}

// dryFramesWorthSaying is how long the buffer has to be empty before it is a
// gap in the audio rather than the space between two frames.
const dryFramesWorthSaying = 3

// sinceDrain is how long ago the queue was last emptied on purpose, or -1 when
// it never was. A gap whose length is about this began at the drain; a gap much
// shorter than it did not.
func sinceDrain(stamp *atomic.Int64) int64 {
	at := stamp.Load()
	if at == 0 {
		return -1
	}
	return time.Now().UnixMilli() - at
}

// noteHeld counts a frame kept back for a DAVE epoch change, saying so once per
// change.
func (s *ConnSender) noteHeld() {
	s.statsMu.Lock()
	s.heldFrames++
	first := !s.holding
	s.holding = true
	s.statsMu.Unlock()
	if first {
		s.logger.Info("voice: holding frames while the DAVE epoch changes")
	}
}

// takeHolding reports whether the last pull held its frames back, and how many
// it kept: the run is over, so the next hold says so again.
func (s *ConnSender) takeHolding() (bool, int64) {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	held, frames := s.holding, s.heldFrames
	s.holding, s.heldFrames = false, 0
	return held, frames
}

// countDry is one pull that found nothing queued.
func (s *ConnSender) countDry() {
	s.statsMu.Lock()
	s.dry++
	s.statsMu.Unlock()
}

// noteDryGap closes a run of empty pulls and says how long it was: a buffer
// that ran dry is a gap in the audio, worth saying out loud rather than leaving
// to be heard.
func (s *ConnSender) noteDryGap() {
	s.statsMu.Lock()
	frames := s.dry
	if frames == 0 {
		s.statsMu.Unlock()
		return
	}
	s.dry = 0
	s.gaps++
	if frames > s.worstFrames {
		s.worstFrames = frames
	}
	gaps, worst := s.gaps, s.worstFrames
	s.statsMu.Unlock()

	if frames >= dryFramesWorthSaying {
		s.logger.Warn("voice: the audio buffer ran dry",
			"frames", frames, "ms", frames*frameMs, "gaps", gaps, "worst_ms", worst*frameMs,
			"since_drain_ms", sinceDrain(&s.drainedAt))
	} else {
		// Short enough to be a click rather than a hole, and still worth
		// counting: a run of these is what choppy sounds like.
		s.logger.Debug("voice: the audio buffer briefly empty",
			"frames", frames, "ms", frames*frameMs, "gaps", gaps)
	}
}

// pullProvider is one install of the connection's frame provider: its Close
// only ends that install, so a replaced loop cannot close the sender under the
// one that took over.
type pullProvider struct {
	sender *ConnSender
	live   chan struct{}
	once   sync.Once
}

func (p *pullProvider) ProvideOpusFrame() ([]byte, error) { return p.sender.provide(p.live) }

func (p *pullProvider) Close() { p.once.Do(func() { close(p.live) }) }

var (
	_ Sender                  = (*ConnSender)(nil)
	_ voice.OpusFrameProvider = (*pullProvider)(nil)
)
