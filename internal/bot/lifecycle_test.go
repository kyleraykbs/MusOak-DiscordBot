package bot

import (
	"context"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"

	"github.com/kyleraykbs/MusOak/pkg/client"

	"github.com/kyleraykbs/MusOak-DiscordBot/internal/config"
	"github.com/kyleraykbs/MusOak-DiscordBot/internal/state"
)

// discarded is a logger with nowhere to write, for the parts of a bot a test
// builds by hand.
func discarded() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// The placeholder account these tests log in as. The room tests register the
// same name and password on musoakd, so the bot's login is not what fails.
const (
	testPlaceholderUser     = "discord-bot"
	testPlaceholderPassword = "secret"
)

// testBot is a Bot with its own state file and a Discord session that never
// connects. serverURL is where its musoak calls go.
func testBot(t *testing.T, serverURL string, store *state.Store) *Bot {
	t.Helper()
	if store == nil {
		var err error
		store, err = state.Open(filepath.Join(t.TempDir(), "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
	}
	cache, err := client.OpenCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Token:         "test",
		ServerURL:     serverURL,
		Account:       config.Account{Username: testPlaceholderUser, Password: testPlaceholderPassword, DisplayName: "Discord Bot"},
		AudioBufferMs: config.DefaultAudioBufferMs,
		AudioVolume:   config.DefaultVolume,
		CacheDir:      t.TempDir(),
	}
	b, err := New(cfg, store, cache, discarded())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	return b
}

// blockingConn is a voice connection whose handshake never finishes, which is
// what a voice gateway that stopped answering looks like.
type blockingConn struct {
	voice.Conn
	opened chan struct{}
}

func (c *blockingConn) ChannelID() *snowflake.ID { return nil }
func (c *blockingConn) Open(ctx context.Context, channelID snowflake.ID, selfMute, selfDeaf bool) error {
	close(c.opened)
	<-ctx.Done()
	return ctx.Err()
}

// fakeManager is the voice manager as the bot's join and shutdown use it: it
// hands back one connection and records that it was closed.
type fakeManager struct {
	conn   voice.Conn
	closed chan struct{}
	once   sync.Once
}

func newFakeManager(conn voice.Conn) *fakeManager {
	return &fakeManager{conn: conn, closed: make(chan struct{})}
}

func (m *fakeManager) HandleVoiceStateUpdate(gateway.EventVoiceStateUpdate)   {}
func (m *fakeManager) HandleVoiceServerUpdate(gateway.EventVoiceServerUpdate) {}
func (m *fakeManager) CreateConn(snowflake.ID) voice.Conn                     { return m.conn }
func (m *fakeManager) GetConn(snowflake.ID) voice.Conn                        { return m.conn }
func (m *fakeManager) Conns() iter.Seq[voice.Conn]                            { return func(func(voice.Conn) bool) {} }
func (m *fakeManager) RemoveConn(snowflake.ID)                                {}
func (m *fakeManager) Close(ctx context.Context) {
	m.once.Do(func() { close(m.closed) })
}

var _ voice.Manager = (*fakeManager)(nil)

// withVoice gives a hand-built bot a voice hub whose manager is the fake.
func withVoice(b *Bot, manager *fakeManager) {
	b.voice = newVoiceHub(b.session, b.logger)
	b.voice.manager = manager
}

// The watchdog in a run's voice sender is a goroutine and a timer of its own,
// and nothing else ends it: a run that is stopped has to close it, or every
// join and leave leaves one ticking for the life of the process.
func TestStoppingAParticipantClosesItsVoiceSender(t *testing.T) {
	b := testBot(t, "http://127.0.0.1:1", nil)

	g := b.guild("g1")
	done := make(chan struct{})
	close(done)
	var closed int
	var roomID string
	g.run = &participantRun{
		roomID:    "room-1",
		cancel:    func() { roomID = "cancelled" },
		done:      done,
		stopAudio: func() { closed++ },
	}

	b.stopParticipant(g)
	if roomID != "cancelled" {
		t.Error("the run's context was not cancelled")
	}
	if closed != 1 {
		t.Errorf("the voice sender was closed %d times, want once", closed)
	}
	if g.run != nil {
		t.Error("the guild still holds the stopped run")
	}
}

// Shutdown used to stop the participants and close the gateway, and leave the
// voice connections up: the bot stayed in the channel until Discord timed it
// out, and its voice gateways and UDP sockets were never hung up.
func TestCloseLeavesVoiceAndEndsEveryParticipant(t *testing.T) {
	manager := newFakeManager(nil)
	b := testBot(t, "http://127.0.0.1:1", nil)
	withVoice(b, manager)

	var mu sync.Mutex
	stopped := map[string]bool{}
	for _, guildID := range []string{"g1", "g2"} {
		g := b.guild(guildID)
		done := make(chan struct{})
		close(done)
		id := guildID
		g.run = &participantRun{roomID: "room-" + id, cancel: func() {}, done: done,
			stopAudio: func() { mu.Lock(); stopped[id] = true; mu.Unlock() }}
	}

	b.Close()

	mu.Lock()
	defer mu.Unlock()
	if !stopped["g1"] || !stopped["g2"] {
		t.Errorf("participants left running: %v", stopped)
	}
	select {
	case <-manager.closed:
	case <-time.After(time.Second):
		t.Error("shutdown never closed the voice manager: the bot is still in the call")
	}
}

// A join waits on Discord's voice handshake and on the room server, and both
// of those are somebody else answering. An answer that never comes used to
// leave the command thinking forever, with a half-built connection left in the
// manager and nothing to retry.
func TestAJoinThatNeverAnswersIsGivenUp(t *testing.T) {
	restore := voiceJoinTimeout
	voiceJoinTimeout = 250 * time.Millisecond
	t.Cleanup(func() { voiceJoinTimeout = restore })

	b := testBot(t, "http://127.0.0.1:1", nil)
	conn := &blockingConn{opened: make(chan struct{})}
	withVoice(b, newFakeManager(conn))
	// A room the store already knows, so the voice leg is what the join waits
	// on rather than the room lookup.
	const guild, channel = "1341810581988048929", "1514564335521894494"
	if err := b.store.SetRoom(context.Background(), guild, channel, "room-1"); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	_, err := b.startGuild(context.Background(), guild, channel)
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("the join reported success with a handshake that never finished")
	}
	if elapsed > 5*time.Second {
		t.Errorf("the join took %s: nothing bounded it", elapsed)
	}
	select {
	case <-conn.opened:
	default:
		t.Error("the join never reached the voice handshake")
	}
}

// setDisplayName is the one call the bot makes without the SDK, and it runs on
// the startup path: a server that accepts the connection and then never answers
// used to hang startup there.
func TestADisplayNameThatNeverAnswersGivesUp(t *testing.T) {
	saved := apiClient.Timeout
	apiClient.Timeout = 300 * time.Millisecond
	t.Cleanup(func() { apiClient.Timeout = saved })

	release := make(chan struct{})
	var releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auth/login" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"token":"t","user":{"id":"u1"}}`)
			return
		}
		// The PATCH this test is about: accepted, and never answered.
		<-release
	}))
	// httptest's Close waits for the handler it left running, so the handler is
	// let go first — defers run last-in-first-out — and again at the end, which
	// the once makes harmless.
	defer server.Close()
	defer letGo()

	a := newAccounts(&config.Config{
		ServerURL: server.URL,
		Account:   config.Account{Username: "bot", Password: "p", DisplayName: "Discord Bot"},
	}, nil, discarded())

	started := time.Now()
	err := a.setDisplayName(context.Background())
	elapsed := time.Since(started)
	letGo()

	if err == nil {
		t.Fatal("the display name was set by a server that never answered")
	}
	if elapsed > 5*time.Second {
		t.Errorf("setting the display name took %s: nothing bounded it", elapsed)
	}
}
