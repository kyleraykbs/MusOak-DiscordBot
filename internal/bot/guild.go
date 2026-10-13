package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	disgovoice "github.com/disgoorg/disgo/voice"

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
	conn, err := b.voice.open(ctx, guildID, channelID)
	if err != nil {
		return "", err
	}
	if err := b.startParticipant(g, guildID, channelID, roomID, conn); err != nil {
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

	name := b.channelName(channelID)

	// A room with this channel's name may already be there: the bot was
	// restarted with a fresh state, or the room was left behind. Rejoining it
	// is what keeps one voice channel from having two rooms with its name.
	if existing, ok := b.existingRoomFor(ctx, name); ok {
		if err := b.store.SetRoom(ctx, guildID, channelID, existing); err != nil {
			return "", fmt.Errorf("remembering the room: %w", err)
		}
		b.logger.Info("bot: joining the room this channel already has",
			"guild", guildID, "channel", channelID, "room", existing, "name", name)
		return existing, nil
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

// channelName is what the room for a voice channel is called. Without a
// gateway state to ask — in tests — the channel's own id stands in.
func (b *Bot) channelName(channelID string) string {
	if b.session == nil || b.session.State == nil {
		return channelID
	}
	if channel, err := b.session.State.Channel(channelID); err == nil && channel.Name != "" {
		return channel.Name
	}
	return channelID
}

// existingRoomFor finds a room this channel should use instead of a new one:
// one already named after it.
func (b *Bot) existingRoomFor(ctx context.Context, name string) (string, bool) {
	if name == "" {
		return "", false
	}
	var rooms []client.Room
	err := b.accounts.withPlaceholder(ctx, func(c *client.Client) error {
		var err error
		rooms, err = c.Rooms(ctx)
		return err
	})
	if err != nil {
		b.logger.Warn("bot: could not list the rooms", "error", err)
		return "", false
	}
	return pickRoomByName(rooms, name, b.accounts.placeholderID())
}

// pickRoomByName chooses the room a voice channel belongs to when nothing is
// remembered for it. The bot being in a room with that name is the strongest
// signal — it is the room the bot made — then a room somebody is actually
// listening in, then the newest of them.
func pickRoomByName(rooms []client.Room, name, placeholder string) (string, bool) {
	best := ""
	var bestIn, bestLive bool
	var bestAt int64
	for _, room := range rooms {
		if room.Name != name {
			continue
		}
		in := placeholder != "" && room.HasMember(placeholder)
		live := len(room.Members) > 0
		if best == "" || betterRoom(in, live, room.CreatedAtMs, bestIn, bestLive, bestAt) {
			best, bestIn, bestLive, bestAt = room.ID, in, live, room.CreatedAtMs
		}
	}
	return best, best != ""
}

// betterRoom ranks two candidates for the room a channel should use.
func betterRoom(in, live bool, at int64, bestIn, bestLive bool, bestAt int64) bool {
	if in != bestIn {
		return in
	}
	if live != bestLive {
		return live
	}
	return at > bestAt
}

// joinRoomAsPlaceholder joins the room as the placeholder. A room the server
// no longer has — it closed while nobody was in it — is not an error to hand
// the user: the mapping is forgotten, a fresh room is opened for the channel
// and this member joins that instead.
func (b *Bot) joinRoomAsPlaceholder(ctx context.Context, guildID, channelID, roomID string) (*client.RoomClient, error) {
	var room *client.RoomClient
	err := b.accounts.withPlaceholder(ctx, func(c *client.Client) error {
		var err error
		room, err = c.JoinRoom(ctx, roomID)
		return err
	})
	if err == nil {
		b.restoreDiscordQueue(ctx, room)
		return room, nil
	}
	if !client.IsNotFound(err) {
		return nil, fmt.Errorf("joining room %s: %w", roomID, err)
	}

	b.logger.Info("bot: the mapped room is gone; opening a new one", "room", roomID)
	if forgetErr := b.store.ForgetRoom(ctx, guildID, channelID); forgetErr != nil {
		b.logger.Warn("bot: forgetting a gone room", "error", forgetErr)
	}
	fresh, createErr := b.ensureRoom(ctx, guildID, channelID)
	if createErr != nil {
		return nil, createErr
	}
	err = b.accounts.withPlaceholder(ctx, func(c *client.Client) error {
		var err error
		room, err = c.JoinRoom(ctx, fresh)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("joining the new room %s: %w", fresh, err)
	}
	return room, nil
}

// restoreDiscordQueue puts back the songs somebody queued through Discord when
// the room no longer holds them.
//
// A member who leaves takes their queue with them, and the server removes a
// member whose event socket was away past its grace - so a blink costs the
// placeholder everything queued from Discord, which is a queue nobody can
// retype. The bot remembers what it queued and for whom, so rejoining puts it
// back in the same order, under the same Discord user.
func (b *Bot) restoreDiscordQueue(ctx context.Context, room *client.RoomClient) {
	rows, err := b.store.QueuedForRoom(ctx, room.RoomID)
	if err != nil || len(rows) == 0 {
		return
	}
	snapshot, err := room.Snapshot(ctx)
	if err != nil {
		return
	}
	present := map[string]bool{}
	for _, item := range snapshot.Queues[room.MemberID] {
		present[item.TrackID] = true
	}
	missing, forget := planRestore(rows, present)
	if len(missing) == 0 {
		if len(forget) > 0 {
			_ = b.store.ForgetQueued(ctx, room.RoomID, forget...)
		}
		return
	}

	trackIDs := make([]string, 0, len(missing))
	for _, row := range missing {
		trackIDs = append(trackIDs, row.TrackID)
	}
	after, err := room.QueueMany(ctx, trackIDs)
	if err != nil {
		b.logger.Warn("bot: could not put the Discord queue back", "room", room.RoomID, "error", err)
		return
	}
	// The old items are gone with the membership; the new ones are these.
	if err := b.store.ForgetQueued(ctx, room.RoomID, forget...); err != nil {
		b.logger.Warn("bot: forgetting restored items", "error", err)
	}
	queued := after.Queues[room.MemberID]
	byTrack := map[string][]client.QueueItem{}
	for _, item := range queued {
		byTrack[item.TrackID] = append(byTrack[item.TrackID], item)
	}
	restored := 0
	for _, row := range missing {
		items := byTrack[row.TrackID]
		if len(items) == 0 {
			continue
		}
		item := items[0]
		byTrack[row.TrackID] = items[1:]
		if err := b.store.RememberQueued(ctx, state.Queued{
			RoomID: room.RoomID, ItemID: item.ID, DiscordID: row.DiscordID, TrackID: item.TrackID,
		}); err != nil {
			b.logger.Warn("bot: re-attributing a restored song", "error", err)
		}
		restored++
	}
	b.logger.Info("bot: put the Discord queue back after a drop",
		"room", room.RoomID, "songs", restored)
}

// planRestore decides what a re-join has to put back: the attributed songs the
// room no longer holds (they went with the membership) and the attribution
// rows whose items are gone. Pure, so the decision is testable on its own.
func planRestore(rows []state.Queued, present map[string]bool) (missing []state.Queued, forget []string) {
	for _, row := range rows {
		if present[row.TrackID] {
			continue // still queued: the room kept it
		}
		missing = append(missing, row)
		forget = append(forget, row.ItemID)
	}
	return missing, forget
}

// startParticipant attaches a fresh participant — the placeholder's
// membership, a voice sink, and the play tracker — and runs it until the room
// closes or it is replaced.
func (b *Bot) startParticipant(g *guildState, guildID, channelID, roomID string, conn disgovoice.Conn) error {
	room, err := b.joinRoomAsPlaceholder(context.Background(), guildID, channelID, roomID)
	if err != nil {
		return err
	}

	bufferFrames := b.cfg.AudioBufferMs / voice.FrameMs
	if bufferFrames < 3 {
		bufferFrames = 3
	}
	sink := voice.NewSink("ffmpeg", voice.NewConnSender(conn, bufferFrames, b.logger, b.voice.shouldHoldFrames), b.logger)
	bitrate := b.cfg.AudioBitrateKbps
	if bitrate <= 0 {
		bitrate = channelBitrateKbps(b.session, channelID)
	}
	sink.SetBitrate(bitrate)
	sink.SetVolume(b.cfg.AudioVolume)
	b.logger.Info("bot: playing into the channel",
		"channel", channelID, "bitrate_kbps", bitrate, "volume", b.cfg.AudioVolume,
		"buffer_ms", bufferFrames*voice.FrameMs)
	participant := client.NewParticipant(room, b.cache, sink, b.logger)
	// The buffer is a delay by design: the tolerance has to sit past it, or
	// every beat corrects the buffer with a seek and the audio tears.
	participant.SetDriftTolerance(int64(bufferFrames)*voice.FrameMs + 2000)
	tracker := newPlayTracker(b.store, roomID, b.logger)
	participant.OnEvent = tracker.event

	ctx, cancel := context.WithCancel(b.ctx)
	run := &participantRun{roomID: roomID, cancel: cancel, done: make(chan struct{})}

	g.mu.Lock()
	g.voiceChannel = channelID
	g.run = run
	g.mu.Unlock()

	// How far behind the room the audio actually is: the buffer is a delay by
	// design, and this is the number to watch when it stops feeling like one.
	go func() {
		ticker := time.NewTicker(lagReportEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				local, ok := participant.LocalPosition(context.Background())
				if !ok {
					continue
				}
				b.logger.Debug("bot: room clock and local audio",
					"room_ms", participant.Position(), "local_ms", local,
					"behind_ms", participant.Position()-local)
			}
		}
	}()

	go func() {
		defer close(run.done)
		backoff := participantRetryFloor
		for {
			err := participant.Run(ctx)
			tracker.flush()
			if errors.Is(err, context.Canceled) {
				return // replaced or stopped
			}
			if errors.Is(err, client.ErrRoomClosed) {
				b.leaveClosedRoom(g, run, guildID, channelID, roomID)
				return
			}
			// A dropped socket is not a closed room: the room is still there
			// and this member was in it (the server's grace may have taken
			// them out while the socket was away), so re-join and follow it
			// again, backing off as failures repeat. Leaving the voice channel
			// here is what a shuffle looked like: one lost socket and the bot
			// walked out of a room that never went anywhere.
			b.logger.Warn("bot: the room follower stopped; rejoining",
				"room", roomID, "error", err, "in", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < participantRetryCeiling {
				backoff *= 2
			}
			rejoinErr := b.accounts.withPlaceholder(ctx, func(c *client.Client) error {
				_, err := c.JoinRoom(ctx, roomID)
				return err
			})
			if client.IsNotFound(rejoinErr) {
				// The room really did close while we were away.
				b.leaveClosedRoom(g, run, guildID, channelID, roomID)
				return
			}
			if rejoinErr != nil {
				b.logger.Warn("bot: could not re-join the room", "room", roomID, "error", rejoinErr)
			}
		}
	}()
	return nil
}

// channelBitrateKbps is what a voice channel allows, in kbps: Discord caps the
// encoder there, so it is both the ceiling and the target. Zero when the
// channel is not known yet, which the encoder reads as its default.
func channelBitrateKbps(session *discordgo.Session, channelID string) int {
	channel, err := session.State.Channel(channelID)
	if err != nil || channel == nil {
		return 0
	}
	return voiceBitrateKbps(channel.Bitrate)
}

// voiceBitrateKbps clamps a channel bitrate (bits per second) into what Discord
// accepts for voice: 8kbps at the floor, 256kbps at the ceiling.
func voiceBitrateKbps(bitsPerSecond int) int {
	if bitsPerSecond <= 0 {
		return 0
	}
	kbps := bitsPerSecond / 1000
	if kbps < 8 {
		return 8
	}
	if kbps > voice.MaxVoiceBitrateKbps {
		return voice.MaxVoiceBitrateKbps
	}
	return kbps
}

// leaveClosedRoom gives the voice channel back and forgets the mapping, so the
// next command opens a fresh room.
func (b *Bot) leaveClosedRoom(g *guildState, run *participantRun, guildID, channelID, roomID string) {
	b.logger.Info("bot: the room closed", "room", roomID)
	g.mu.Lock()
	if g.run == run {
		g.run = nil
		g.voiceChannel = ""
	}
	g.mu.Unlock()
	b.voice.close(context.Background(), guildID)
	if err := b.store.ForgetRoom(context.Background(), guildID, channelID); err != nil {
		b.logger.Warn("bot: forgetting a closed room", "error", err)
	}
}

// lagReportEvery is how often the bot says how far behind the room its audio
// is running.
const lagReportEvery = 10 * time.Second

// How long a lost room follower waits before trying again, and how far the
// wait grows when failures repeat.
//
// The floor is short on purpose: the server's socket grace is ten seconds, so
// a follower that comes back inside it never leaves the room at all. A ten
// second first wait meant the member was removed before the retry, which is
// the bot dropping out of the listening board and hopping back in later.
const (
	participantRetryFloor   = time.Second
	participantRetryCeiling = 30 * time.Second
)

// leaveVoice stops the participant, disconnects from the guild's voice and
// leaves the room as the placeholder, so the listening board does not keep a
// bot that is not there. The room itself survives while anybody else is in it,
// and its queue with it: /join comes back to the same room.
func (b *Bot) leaveVoice(ctx context.Context, guildID string) {
	g := b.guild(guildID)
	g.mu.Lock()
	roomID := ""
	if g.run != nil {
		roomID = g.run.roomID
	}
	g.mu.Unlock()

	b.stopParticipant(g)
	g.mu.Lock()
	g.voiceChannel = ""
	g.mu.Unlock()
	b.voice.close(ctx, guildID)

	if roomID == "" {
		return
	}
	err := b.accounts.withPlaceholder(ctx, func(c *client.Client) error {
		room, err := c.JoinRoom(ctx, roomID)
		if err != nil {
			return err
		}
		return room.Leave(ctx)
	})
	if err != nil && !client.IsNotFound(err) {
		b.logger.Warn("bot: could not leave the room", "room", roomID, "error", err)
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
	// The song has been heard, so it is not something a re-join should put
	// back into the queue.
	if err := t.store.ForgetQueued(context.Background(), t.roomID, song.itemID); err != nil {
		t.logger.Warn("tracker: forget a played queue item", "error", err)
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
