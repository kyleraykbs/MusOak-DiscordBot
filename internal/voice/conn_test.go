package voice

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	botgateway "github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave"
	"github.com/disgoorg/snowflake/v2"
)

// fakeConn is the slice of a voice connection the sender touches: it takes the
// frame provider and nothing else.
type fakeConn struct {
	provider voice.OpusFrameProvider
}

func (f *fakeConn) Gateway() voice.Gateway                { return nil }
func (f *fakeConn) UDP() voice.UDPConn                    { return nil }
func (f *fakeConn) DAVE() godave.Session                  { return nil }
func (f *fakeConn) ChannelID() *snowflake.ID              { return nil }
func (f *fakeConn) GuildID() snowflake.ID                 { return 0 }
func (f *fakeConn) UserIDBySSRC(ssrc uint32) snowflake.ID { return 0 }
func (f *fakeConn) SetSpeaking(ctx context.Context, flags voice.SpeakingFlags) error {
	return nil
}
func (f *fakeConn) SetOpusFrameProvider(provider voice.OpusFrameProvider) { f.provider = provider }
func (f *fakeConn) SetOpusFrameReceiver(receiver voice.OpusFrameReceiver) {}
func (f *fakeConn) SetEventHandlerFunc(handler voice.EventHandlerFunc)    {}
func (f *fakeConn) Open(ctx context.Context, channelID snowflake.ID, selfMute, selfDeaf bool) error {
	return nil
}
func (f *fakeConn) Close(ctx context.Context)                                        {}
func (f *fakeConn) HandleVoiceStateUpdate(update botgateway.EventVoiceStateUpdate)   {}
func (f *fakeConn) HandleVoiceServerUpdate(update botgateway.EventVoiceServerUpdate) {}

var _ voice.Conn = (*fakeConn)(nil)

func TestConnSenderHandsFramesToTheConn(t *testing.T) {
	conn := &fakeConn{}
	sender := NewConnSender(conn, 4)
	if conn.provider == nil {
		t.Fatal("the connection was not given a frame provider")
	}

	frame := []byte{0xAA, 0xBB}
	if err := sender.Send(frame); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got, err := conn.provider.ProvideOpusFrame()
	if err != nil || !bytes.Equal(got, frame) {
		t.Fatalf("pulled %x (%v), want the frame sent", got, err)
	}

	// Nothing queued: an empty frame, which the connection's sender turns into
	// silence and then stops speaking. Not an error, and not a stall.
	got, err = conn.provider.ProvideOpusFrame()
	if err != nil || len(got) != 0 {
		t.Fatalf("idle pull = %x (%v), want an empty frame", got, err)
	}
}

func TestConnSenderDrainDropsQueuedFrames(t *testing.T) {
	conn := &fakeConn{}
	sender := NewConnSender(conn, 4)
	for i := range 4 {
		if err := sender.Send([]byte{byte(i)}); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}
	sender.Drain()
	if got, _ := conn.provider.ProvideOpusFrame(); len(got) != 0 {
		t.Fatalf("after Drain: %x, want nothing left", got)
	}
}

func TestConnSenderCountsWhatLeftForTheWire(t *testing.T) {
	conn := &fakeConn{}
	sender := NewConnSender(conn, 8)
	for i := range 5 {
		if err := sender.Send([]byte{byte(i)}); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}
	// Pushed is not played: the buffer ahead of the wire is not audible yet,
	// and a sink that counted it would report position early.
	if got := sender.PlayedFrames(); got != 0 {
		t.Fatalf("played before any pull = %d, want 0", got)
	}
	for i := range 3 {
		if _, err := conn.provider.ProvideOpusFrame(); err != nil {
			t.Fatalf("pull %d: %v", i, err)
		}
	}
	if got := sender.PlayedFrames(); got != 3 {
		t.Fatalf("played after three pulls = %d, want 3", got)
	}
}

func TestConnSenderClosesCleanly(t *testing.T) {
	conn := &fakeConn{}
	sender := NewConnSender(conn, 4)

	// The connection's sender closes the provider when the connection goes
	// away; the pull then reports EOF so the sender's loop can let go.
	sender.Close()
	if _, err := conn.provider.ProvideOpusFrame(); !errors.Is(err, io.EOF) {
		t.Fatalf("pull after close = %v, want EOF", err)
	}
	if err := sender.Send([]byte{1}); err == nil {
		t.Fatal("Send on a closed sender succeeded")
	}
	sender.Close() // idempotent
}
