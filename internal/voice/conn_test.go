package voice

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	botgateway "github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave"
	"github.com/disgoorg/snowflake/v2"
)

// fakeConn is the slice of a voice connection the sender touches: it takes the
// frame provider and nothing else.
type fakeConn struct {
	mu       sync.Mutex
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
func (f *fakeConn) SetOpusFrameProvider(provider voice.OpusFrameProvider) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.provider = provider
}

// providerFor is the install the connection currently holds, for tests: the
// sender gives it up and replaces it from its own goroutine.
func (f *fakeConn) providerFor() voice.OpusFrameProvider {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.provider
}
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
	sender := NewConnSender(conn, 4, nil, nil, nil)
	if conn.providerFor() == nil {
		t.Fatal("the connection was not given a frame provider")
	}

	frame := []byte{0xAA, 0xBB}
	if err := sender.Send(frame); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got, err := conn.providerFor().ProvideOpusFrame()
	if err != nil || !bytes.Equal(got, frame) {
		t.Fatalf("pulled %x (%v), want the frame sent", got, err)
	}

	// Nothing queued: an empty frame, which the connection's sender turns into
	// silence and then stops speaking. Not an error, and not a stall.
	got, err = conn.providerFor().ProvideOpusFrame()
	if err != nil || len(got) != 0 {
		t.Fatalf("idle pull = %x (%v), want an empty frame", got, err)
	}
}

func TestConnSenderDrainDropsQueuedFrames(t *testing.T) {
	conn := &fakeConn{}
	sender := NewConnSender(conn, 4, nil, nil, nil)
	for i := range 4 {
		if err := sender.Send([]byte{byte(i)}); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}
	sender.Drain()
	if got, _ := conn.providerFor().ProvideOpusFrame(); len(got) != 0 {
		t.Fatalf("after Drain: %x, want nothing left", got)
	}
}

func TestConnSenderCountsWhatLeftForTheWire(t *testing.T) {
	conn := &fakeConn{}
	sender := NewConnSender(conn, 8, nil, nil, nil)
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
		if _, err := conn.providerFor().ProvideOpusFrame(); err != nil {
			t.Fatalf("pull %d: %v", i, err)
		}
	}
	if got := sender.PlayedFrames(); got != 3 {
		t.Fatalf("played after three pulls = %d, want 3", got)
	}
}

// While the DAVE group changes, a frame sent now is encrypted under an epoch
// the listeners have left: it is a dropout either way, so the frames wait in
// the buffer instead of being thrown away.
func TestFramesWaitOutADaveEpochChange(t *testing.T) {
	conn := &fakeConn{}
	var holding atomic.Bool
	holding.Store(true)
	sender := NewConnSender(conn, 8, nil, func() bool { return holding.Load() }, nil)

	for i := range 4 {
		if err := sender.Send([]byte{byte(i)}); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}
	// Mid-change: silence goes out, and nothing is consumed.
	if got, err := conn.providerFor().ProvideOpusFrame(); err != nil || len(got) != 0 {
		t.Fatalf("pull while holding = %x (%v), want silence", got, err)
	}
	if played := sender.PlayedFrames(); played != 0 {
		t.Fatalf("played while holding = %d, want 0", played)
	}

	// The epoch settles: every frame is still there, in order.
	holding.Store(false)
	for i := range 4 {
		got, err := conn.providerFor().ProvideOpusFrame()
		if err != nil || len(got) != 1 || got[0] != byte(i) {
			t.Fatalf("pull %d after the change = %x (%v)", i, got, err)
		}
	}
	if played := sender.PlayedFrames(); played != 4 {
		t.Fatalf("played after the change = %d, want 4", played)
	}
}

// A connection whose sender died (a closed socket, a voice gateway that went
// away) stops pulling for good: frames wait, the audio stops, and the room runs
// on. The watchdog re-installs the provider, which is how a fresh pull loop
// starts.
func TestTheSenderIsRestartedWhenTheConnectionStopsPulling(t *testing.T) {
	conn := &fakeConn{}
	sender := NewConnSender(conn, 8, nil, nil, nil)
	sender.tune(50*time.Millisecond, 100)

	for i := range 8 {
		if err := sender.Send([]byte{byte(i)}); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}
	first := conn.providerFor()
	if _, err := first.ProvideOpusFrame(); err != nil {
		t.Fatalf("pull: %v", err)
	}

	// The connection stops pulling with frames still queued.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if conn.providerFor() != first {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if conn.providerFor() == first {
		t.Fatal("the sender was never restarted")
	}
	// And the fresh install serves the frames that were waiting.
	if _, err := conn.providerFor().ProvideOpusFrame(); err != nil {
		t.Fatalf("pull after restart: %v", err)
	}
}

// A connection that stopped taking audio is one whose own voice socket is gone,
// and re-installing a frame provider into it cannot help: the sender asks for a
// rebuilt connection — a fresh gateway, socket and session, and a re-join — and
// follows the audio onto it. The install that served the dead one is let go, so
// nothing keeps taking frames out of the queue on its way out.
func TestAStalledConnectionIsRebuilt(t *testing.T) {
	conn := &fakeConn{}
	fresh := &fakeConn{}
	rebuilt := 0
	sender := NewConnSender(conn, 8, nil, nil, func(ctx context.Context) (voice.Conn, error) {
		rebuilt++
		return fresh, nil
	})
	sender.tune(20*time.Millisecond, 2)

	for i := range 8 {
		if err := sender.Send([]byte{byte(i)}); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}

	waitFor(t, 2*time.Second, "the connection to be rebuilt", func() bool { return fresh.providerFor() != nil })
	if rebuilt != 1 {
		t.Fatalf("the sender asked for %d rebuilds, want 1", rebuilt)
	}
	if got, err := fresh.providerFor().ProvideOpusFrame(); err != nil || len(got) != 1 || got[0] != 0 {
		t.Fatalf("pull from the rebuilt connection = %x (%v), want the first waiting frame", got, err)
	}
	if _, err := conn.providerFor().ProvideOpusFrame(); !errors.Is(err, io.EOF) {
		t.Fatalf("pull from the dead connection = %v, want EOF (its install is let go)", err)
	}
}

// dyingConn takes exactly one frame from each provider it is handed and then
// goes quiet, which is what a sender whose UDP socket has closed does: the
// install dies on its first frame. One frame per install is not a connection
// that is pulling, so a sender that keeps re-installing into it re-installs
// forever — this must be rebuilt instead.
type dyingConn struct {
	fakeConn
}

func (d *dyingConn) SetOpusFrameProvider(provider voice.OpusFrameProvider) {
	d.fakeConn.SetOpusFrameProvider(provider)
	_, _ = provider.ProvideOpusFrame()
}

func TestAConnectionThatTakesOneFramePerInstallIsRebuilt(t *testing.T) {
	conn := &dyingConn{}
	fresh := &fakeConn{}
	sender := NewConnSender(conn, 8, nil, nil, func(ctx context.Context) (voice.Conn, error) {
		return fresh, nil
	})
	sender.tune(200*time.Millisecond, 2)

	for i := range 8 {
		if err := sender.Send([]byte{byte(i)}); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}

	waitFor(t, 3*time.Second, "the connection to be rebuilt", func() bool { return fresh.providerFor() != nil })
}

// A connection that will not take audio even after a restart must not leave a
// sink waiting on it forever.
func TestASenderThatNeverTakesAudioGivesUp(t *testing.T) {
	conn := &fakeConn{}
	sender := NewConnSender(conn, 8, nil, nil, nil)
	sender.tune(20*time.Millisecond, 2)

	for i := range 8 {
		if err := sender.Send([]byte{byte(i)}); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := sender.Send([]byte{0xFF}); err != nil {
			return // it gave up, as it should
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("a connection that never pulls kept accepting frames")
}

func TestConnSenderClosesCleanly(t *testing.T) {
	conn := &fakeConn{}
	sender := NewConnSender(conn, 4, nil, nil, nil)

	// The connection's sender closes the provider when the connection goes
	// away; the pull then reports EOF so the sender's loop can let go.
	sender.Close()
	if _, err := conn.providerFor().ProvideOpusFrame(); !errors.Is(err, io.EOF) {
		t.Fatalf("pull after close = %v, want EOF", err)
	}
	if err := sender.Send([]byte{1}); err == nil {
		t.Fatal("Send on a closed sender succeeded")
	}
	sender.Close() // idempotent
}
