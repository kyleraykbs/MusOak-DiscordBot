package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"codeberg.org/kyleraykbs/musoak/pkg/client"

	"codeberg.org/kyleraykbs/musoak-discord-bot/internal/state"
	"codeberg.org/kyleraykbs/musoak-discord-bot/internal/voice"
)

// guildState is one Discord server's runtime: where the bot's voice
// connection is and which participant is streaming into it.
type guildState struct {
	mu sync.Mutex
	// voiceChannel is the channel the bot's connection sits in, "" when out.
	voiceChannel string
	run          *participantRun
}

// participantRun is one live participant: the room it follows and how to end it.
type participantRun struct {
	roomID string
	cancel context.CancelFunc
	done   chan struct{}
}

func (r *participantRun) stop() {
	r.cancel()
	<-r.done // nothing else waits on the channel while a lock is held
}

// guild fetches (creating on first use) a guild's runtime.
func (b *Bot) guild(guildID string) *guildState {
	b.mu.Lock()
	defer b.mu.Unlock()
	g, ok := b.guilds[guildID]
	if !ok {
		g = &guildState{}
		b.guilds[guildID] = g
	}
	return g
}

// startGuild puts the bot's voice connection on channelID and its participant
// on that channel's room, creating the room when the channel has none yet. It
// is idempotent: a bot already playing there is left alone.
func (b *Bot) startGuild(ctx context.Context, guildID, channelID string) (string, error) {
	roomID, err := b.ensureRoom(ctx, guildID, channelID)
	if err != nil {
		return "", err
	}

	g := b.guild(guildID)
	g.mu.Lock()
	already := g.voiceChannel == channelID && g.run != nil && g.run.roomID == roomID
	g.mu.Unlock()
	if already {
		return roomID, nil
	}

	// Move the connection first: a participant must never stream into a voice
	// connection that has changed channels under it.
	b.stopParticipant(g)
	vc, err := b.session.ChannelVoiceJoin(guildID, channelID, false, false)
	if err != nil {
		return "", fmt.Errorf("joining the voice channel: %w", err)
	}
	if err := b.startParticipant(g, guildID, channelID, roomID, vc); err != nil {
		return "", err
	}
	return roomID, nil
}

// stopParticipant ends the guild's participant and waits for it to leave. It
// releases the guild lock before waiting, because the participant's own exit
// path takes that lock.
func (b *Bot) stopParticipant(g *guildState) {
	g.mu.Lock()
	run := g.run
	g.run = nil
	g.mu.Unlock()
	if run != nil {
		run.stop()
	}
}

// ensureRoom resolves the voice channel's room, creating and remembering one
// when the channel has never been used.
func (b *Bot) ensureRoom(ctx context.Context, guildID, channelID string) (string, error) {
	roomID, ok, err := b.store.RoomFor(ctx, guildID, channelID)
	if err != nil {
		return "", fmt.Errorf("reading the room mapping: %w", err)
	}
	if ok {
		return roomID, nil
	}

	name := channelID
	if channel, err := b.session.State.Channel(channelID); err == nil && channel.Name != "" {
		name = channel.Name
	}
	var created *client.RoomClient
	err = b.accounts.withPlaceholder(ctx, func(c *client.Client) error {
		var err error
		created, err = c.CreateRoom(ctx, name, "everyone")
		return err
	})
	if err != nil {
		return "", fmt.Errorf("creating the room: %w", err)
	}
	if err := b.store.SetRoom(ctx, guildID, channelID, created.RoomID); err != nil {
		return "", fmt.Errorf("remembering the room: %w", err)
	}
	b.logger.Info("bot: opened a room for a voice channel",
		"guild", guildID, "channel", channelID, "room", created.RoomID, "name", name)
	return created.RoomID, nil
}

// startParticipant attaches a fresh participant — the placeholder's
// membership, a voice sink, and the play tracker — and runs it until the room
// closes or it is replaced.
func (b *Bot) startParticipant(g *guildState, guildID, channelID, roomID string, vc *discordgo.VoiceConnection) error {
	var room *client.RoomClient
	err := b.accounts.withPlaceholder(context.Background(), func(c *client.Client) error {
		var err error
		room, err = c.JoinRoom(context.Background(), roomID)
		return err
	})
	if err != nil {
		return fmt.Errorf("joining room %s: %w", roomID, err)
	}

	sink := voice.NewSink("ffmpeg", voice.NewDiscordSender(vc), b.logger)
	participant := client.NewParticipant(room, b.cache, sink, b.logger)
	tracker := newPlayTracker(b.store, roomID, b.logger)
	participant.OnEvent = tracker.event

	ctx, cancel := context.WithCancel(b.ctx)
	run := &participantRun{roomID: roomID, cancel: cancel, done: make(chan struct{})}

	g.mu.Lock()
	g.voiceChannel = channelID
	g.run = run
	g.mu.Unlock()

	go func() {
		defer close(run.done)
		err := participant.Run(ctx)
		tracker.flush()
		if errors.Is(err, context.Canceled) {
			return
		}
		if err != nil {
			b.logger.Warn("bot: the room follower stopped", "room", roomID, "error", err)
			return
		}
		// The room closed while we were in it: give the voice channel back and
		// forget the mapping so the next command opens a fresh room.
		b.logger.Info("bot: the room closed", "room", roomID)
		b.stopParticipant(g)
		g.mu.Lock()
		if g.voiceChannel == channelID {
			g.voiceChannel = ""
		}
		g.mu.Unlock()
		b.voiceLeave(guildID)
		if err := b.store.ForgetRoom(context.Background(), guildID, channelID); err != nil {
			b.logger.Warn("bot: forgetting a closed room", "error", err)
		}
	}()
	return nil
}

// leaveVoice stops the participant and disconnects from the guild's voice.
func (b *Bot) leaveVoice(guildID string) {
	g := b.guild(guildID)
	b.stopParticipant(g)
	g.mu.Lock()
	g.voiceChannel = ""
	g.mu.Unlock()
	b.voiceLeave(guildID)
}

// voiceLeave disconnects the guild's voice connection, if there is one.
func (b *Bot) voiceLeave(guildID string) {
	if vc, ok := b.session.VoiceConnections[guildID]; ok && vc != nil {
		_ = vc.Disconnect()
	}
}

// stopAll ends every participant; the process is going away.
func (b *Bot) stopAll() {
	b.mu.Lock()
	states := make([]*guildState, 0, len(b.guilds))
	for _, g := range b.guilds {
		states = append(states, g)
	}
	b.mu.Unlock()
	for _, g := range states {
		b.stopParticipant(g)
	}
}

// playTracker attributes the placeholder's listens to whoever queued the
// songs. It watches the room's playback events: an item starting begins a
// measurement, a pause stops it, a different item ends it — and the moment
// listened is written under the Discord user recorded at queue time.
type playTracker struct {
	store  *state.Store
	roomID string
	logger *slog.Logger

	mu      sync.Mutex
	current *playedSong
}

type playedSong struct {
	itemID     string
	trackID    string
	discordID  string
	durationMs int64
	startedAt  time.Time
	listened   time.Duration
	paused     bool
}

func newPlayTracker(store *state.Store, roomID string, logger *slog.Logger) *playTracker {
	return &playTracker{store: store, roomID: roomID, logger: logger}
}

// event folds a playback event in; every other event type is nothing to do.
func (t *playTracker) event(ev client.Event) {
	if ev.Type != client.EventPlayback {
		return
	}
	var data client.PlaybackEvent
	if err := ev.DecodeData(&data); err != nil {
		t.logger.Warn("tracker: bad playback event", "error", err)
		return
	}
	t.set(data.Current)
}

// set moves the measurement to whatever the room says is on now.
func (t *playTracker) set(current *client.RoomPlayback) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()

	if t.current != nil && (current == nil || current.Item.ID != t.current.itemID) {
		t.recordLocked(now)
	}
	if current == nil {
		return
	}
	if t.current != nil && t.current.itemID == current.Item.ID {
		// The same song: a pause stops the clock, a resume restarts it.
		if current.Paused && !t.current.paused {
			t.current.listened += now.Sub(t.current.startedAt)
			t.current.paused = true
		} else if !current.Paused && t.current.paused {
			t.current.startedAt = now
			t.current.paused = false
		}
		return
	}

	owners, err := t.store.QueuedInRoom(context.Background(), t.roomID)
	if err != nil {
		t.logger.Warn("tracker: queue attribution", "error", err)
	}
	t.current = &playedSong{
		itemID:     current.Item.ID,
		trackID:    current.Item.TrackID,
		discordID:  owners[current.Item.ID],
		durationMs: current.DurationMs,
		startedAt:  now,
	}
}

// flush writes the measurement in progress, called when the participant ends.
func (t *playTracker) flush() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.recordLocked(time.Now())
}

// recordLocked files one listen. It matches the server's own rule: a listen
// shorter than five seconds is not one, and nothing longer than the song is
// believed.
func (t *playTracker) recordLocked(now time.Time) {
	song := t.current
	t.current = nil
	if song == nil || song.discordID == "" {
		return
	}
	listened := song.listened
	if !song.paused {
		listened += now.Sub(song.startedAt)
	}
	ms := listened.Milliseconds()
	if song.durationMs > 0 && ms > song.durationMs {
		ms = song.durationMs
	}
	if ms < 5_000 {
		return
	}
	if err := t.store.RecordPlay(context.Background(), song.discordID, song.trackID, ms, now.UnixMilli()); err != nil {
		t.logger.Warn("tracker: record play", "error", err)
	}
}

// targetRoom is the room a command is about: the one mapped to the invoker's
// voice channel when they are in one, else the room the bot is playing.
func (b *Bot) targetRoom(guildID, userID string) (string, error) {
	if guildID == "" {
		return "", errors.New("this only works in a server")
	}
	if vs, err := b.session.State.VoiceState(guildID, userID); err == nil && vs.ChannelID != "" {
		if roomID, ok, err := b.store.RoomFor(context.Background(), guildID, vs.ChannelID); err == nil && ok {
			return roomID, nil
		}
	}
	g := b.guild(guildID)
	g.mu.Lock()
	run := g.run
	g.mu.Unlock()
	if run != nil {
		return run.roomID, nil
	}
	return "", errors.New("no room here yet — join a voice channel and /join")
}
