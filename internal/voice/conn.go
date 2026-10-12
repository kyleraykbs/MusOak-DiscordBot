package voice

import (
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
	// dry counts the pulls that found nothing queued since the last one that
	// did: a run of them is a gap in the audio, and it is worth saying out
	// loud rather than leaving to be heard. gaps is how many such runs there
	// have been, and worstFrames the longest of them.
	dry         int
	gaps        int
	worstFrames int
	// holding is whether the last pull held, so the transition is logged once.
	holding    bool
	heldFrames int64

	// The watchdog's knobs, so a test can make it quick.
	watchEvery  time.Duration
	giveUpAfter int

	mu   sync.Mutex
	live chan struct{} // the current install's liveness
}

// NewConnSender attaches a frame provider to the connection and returns the
// sender the sink pushes into. bufferFrames is how much audio is kept ahead of
// the wire; anything below a few frames is no buffer at all.
func NewConnSender(conn voice.Conn, bufferFrames int, logger *slog.Logger, hold func() bool) *ConnSender {
	if bufferFrames < 3 {
		bufferFrames = 3
	}
	if logger == nil {
		logger = slog.Default()
	}
	s := &ConnSender{
		conn:        conn,
		queue:       make(chan []byte, bufferFrames),
		logger:      logger,
		hold:        hold,
		done:        make(chan struct{}),
		watchEvery:  2 * time.Second,
		giveUpAfter: 5,
	}
	s.install()
	go s.watch()
	return s
}

// install gives the connection a fresh pull loop over this sender's queue. The
// provider is new each time because the connection closes the one it was
// given when it stops.
func (s *ConnSender) install() {
	live := make(chan struct{})
	s.mu.Lock()
	s.live = live
	s.mu.Unlock()
	s.conn.SetOpusFrameProvider(&pullProvider{sender: s, live: live})
}

// watch notices a connection that has stopped pulling while frames wait, and
// does something about it: re-install, and if that keeps failing, stop
// pretending the connection works.
func (s *ConnSender) watch() {
	ticker := time.NewTicker(s.watchEvery)
	defer ticker.Stop()
	last := s.played.Load()
	stalls := 0
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
		}
		played := s.played.Load()
		if played != last {
			last, stalls = played, 0
			continue
		}
		if len(s.queue) == 0 {
			continue // nothing waiting: quiet is allowed
		}
		stalls++
		if stalls == 1 {
			s.logger.Warn("voice: the connection stopped pulling audio; restarting its sender",
				"queued_frames", len(s.queue))
			s.install()
			continue
		}
		if stalls >= s.giveUpAfter {
			s.logger.Error("voice: the voice connection will not take audio; giving up on it",
				"queued_frames", len(s.queue))
			s.close()
			return
		}
	}
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
	if s.holding {
		s.holding = false
		s.logger.Info("voice: frames flowing again after a DAVE epoch change",
			"held_frames", s.heldFrames)
		s.heldFrames = 0
	}

	select {
	case frame := <-s.queue:
		s.played.Add(1)
		if s.dry > 0 {
			s.gaps++
			if s.dry > s.worstFrames {
				s.worstFrames = s.dry
			}
			if s.dry >= dryFramesWorthSaying {
				s.logger.Warn("voice: the audio buffer ran dry",
					"frames", s.dry, "ms", s.dry*frameMs, "gaps", s.gaps, "worst_ms", s.worstFrames*frameMs)
			} else {
				// Short enough to be a click rather than a hole, and still
				// worth counting: a run of these is what choppy sounds like.
				s.logger.Debug("voice: the audio buffer briefly empty",
					"frames", s.dry, "ms", s.dry*frameMs, "gaps", s.gaps)
			}
		}
		s.dry = 0
		return frame, nil
	case <-live:
		return nil, io.EOF
	case <-s.done:
		return nil, io.EOF
	default:
		s.dry++
		return nil, nil
	}
}

// dryFramesWorthSaying is how long the buffer has to be empty before it is a
// gap in the audio rather than the space between two frames.
const dryFramesWorthSaying = 3

// noteHeld counts a frame kept back for a DAVE epoch change, saying so once per
// change.
func (s *ConnSender) noteHeld() {
	s.heldFrames++
	if s.holding {
		return
	}
	s.holding = true
	s.logger.Info("voice: holding frames while the DAVE epoch changes")
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
