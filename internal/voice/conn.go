package voice

import (
	"errors"
	"io"
	"sync"

	"github.com/disgoorg/disgo/voice"
)

// ConnSender is Sender for a disgo voice connection. Frames hand off through a
// small queue that the connection's own audio sender pulls from once every
// 20ms; the connection wraps them in DAVE and transport encryption on the way
// out, which is the part Discord's v8 voice gateway requires and discordgo's
// own sender never did.
//
// The queue is the pacing: it holds a few frames, so this side stays a little
// ahead of the wire and the connection is never flooded.
type ConnSender struct {
	conn   voice.Conn
	queue  chan []byte
	closed chan struct{}
	once   sync.Once
}

// NewConnSender attaches a frame provider to the connection and returns the
// sender the sink pushes into.
func NewConnSender(conn voice.Conn) *ConnSender {
	s := &ConnSender{
		conn:   conn,
		queue:  make(chan []byte, 4),
		closed: make(chan struct{}),
	}
	conn.SetOpusFrameProvider(s)
	return s
}

// Send queues one opus packet for the connection's sender. The closed check
// comes first on purpose: a select over a closed channel and a writable one
// picks at random, and a frame handed to a gone connection would be a lie.
func (s *ConnSender) Send(packet []byte) error {
	select {
	case <-s.closed:
		return errors.New("voice: the connection is closed")
	default:
	}
	select {
	case <-s.closed:
		return errors.New("voice: the connection is closed")
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

// ProvideOpusFrame is the connection's pull. A frame when one is queued, an
// empty frame when there is nothing to play — the sender turns those into a
// few frames of silence and then stops speaking — and EOF once the sender is
// closed, which is how the pull loop is told to let go.
func (s *ConnSender) ProvideOpusFrame() ([]byte, error) {
	select {
	case frame := <-s.queue:
		return frame, nil
	case <-s.closed:
		return nil, io.EOF
	default:
		return nil, nil
	}
}

// Close ends the pull. The connection's audio sender calls it when the
// connection goes away.
func (s *ConnSender) Close() {
	s.once.Do(func() { close(s.closed) })
}

var (
	_ Sender             = (*ConnSender)(nil)
	_ voice.OpusFrameProvider = (*ConnSender)(nil)
)
