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
	// emptyTimer counts down the grace before leaving a channel nobody is
	// left in.
	emptyTimer *time.Timer
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
	// The songs somebody queued through Discord come along: their attribution
	// moves to the new room, and the restore below puts them back there.
	carried, carryErr := b.store.QueuedForRoom(ctx, roomID)
	if carryErr != nil {
		b.logger.Warn("bot: reading the queue of a gone room", "error", carryErr)
	}
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
	for _, row := range carried {
		if err := b.store.RememberQueued(ctx, state.Queued{
			RoomID: fresh, ItemID: row.ItemID, DiscordID: row.DiscordID, TrackID: row.TrackID,
		}); err != nil {
			b.logger.Warn("bot: carrying a queued song to the new room", "error", err)
		}
	}
	b.restoreDiscordQueue(ctx, room)
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
				// The room closed under this member - the server's own idle
				// rule, or somebody closing it - and the voice channel is
				// still there. A fresh room for the channel keeps the music
				// going; /leave is how the bot is told to stop.
				b.logger.Info("bot: the room closed; opening a fresh one for this channel",
					"room", roomID, "channel", channelID)
				if err := b.store.ForgetRoom(b.ctx, guildID, channelID); err != nil {
					b.logger.Warn("bot: forgetting a closed room", "error", err)
				}
				go func() {
					<-run.done
					if _, err := b.startGuild(b.ctx, guildID, channelID); err != nil {
						b.logger.Warn("bot: could not reopen the room",
							"channel", channelID, "error", err)
					}
				}()
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
				// The room closed while the socket was away, but the voice
				// channel did not: reopening a room for it is the fix, and
				// leaving the channel over a room is what dropped the bot out
				// of the call. The restart waits for this follower to finish,
				// since replacing it is how a fresh room gets a fresh player.
				b.logger.Info("bot: the room closed; opening a fresh one for this channel",
					"room", roomID, "channel", channelID)
				go func() {
					<-run.done
					if _, err := b.startGuild(b.ctx, guildID, channelID); err != nil {
						b.logger.Warn("bot: could not reopen the room",
							"channel", channelID, "error", err)
					}
				}()
				return
			}
			if rejoinErr != nil {
				b.logger.Warn("bot: could not re-join the room", "room", roomID, "error", rejoinErr)
			} else {
				// The member may have been taken out while the socket was away
				// (a longer outage, a restart, a grace that passed): whatever
				// the room lost of this bot's queue is put back.
				b.restoreQueued(ctx, roomID)
			}
		}
	}()
	return nil
}

// missingQueued is the rows the room no longer holds, in the order they were
// queued: what a leave cost, and what has to be put back.
func missingQueued(rows []state.Queued, live map[string]string) []state.Queued {
	missing := make([]state.Queued, 0, len(rows))
	for _, row := range rows {
		if _, held := live[row.ItemID]; held {
			continue
		}
		missing = append(missing, row)
	}
	return missing
}

// restoreQueued puts back the items this bot queued in a room that are no
// longer there.
//
// A member who leaves a room takes their queue with them, and the placeholder
// account holds every unlinked user's songs - so a leave costs those users
// their queue. The bot knows what it queued and under whose name, so what the
// room has lost is queued again, in the order it was asked for.
func (b *Bot) restoreQueued(ctx context.Context, roomID string) {
	rows, err := b.store.QueuedForRoom(ctx, roomID)
	if err != nil {
		b.logger.Warn("bot: could not read the room's queued items", "room", roomID, "error", err)
		return
	}
	if len(rows) == 0 {
		return
	}
	live, err := b.store.QueuedInRoom(ctx, roomID)
	if err != nil {
		b.logger.Warn("bot: could not read the room's live items", "room", roomID, "error", err)
		return
	}
	missing := missingQueued(rows, live)
	if len(missing) == 0 {
		return
	}
	err = b.accounts.withPlaceholder(ctx, func(c *client.Client) error {
		room, err := c.JoinRoom(ctx, roomID)
		if err != nil {
			return err
		}
		added := 0
		for _, row := range missing {
			queued, err := room.Queue(ctx, row.TrackID)
			if err != nil {
				return err
			}
			// The item the room just took is the last one in this member's
			// queue: that is the id the attribution is kept under, so a song
			// starting later is still credited to whoever asked for it.
			items := queued.Queues[room.MemberID]
			if len(items) == 0 {
				continue
			}
			itemID := items[len(items)-1].ID
			if err := b.store.RememberQueued(ctx, state.Queued{
				RoomID: roomID, ItemID: itemID, DiscordID: row.DiscordID, TrackID: row.TrackID,
			}); err != nil {
				b.logger.Warn("bot: could not remember a restored item",
					"room", roomID, "item", itemID, "error", err)
			}
			added++
		}
		b.logger.Info("bot: put back the items the room had lost",
			"room", roomID, "items", added, "missing", len(missing))
		return nil
	})
	if err != nil {
		b.logger.Warn("bot: could not put back the room's queued items",
			"room", roomID, "error", err)
	}
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

// lagReportEvery is how often the bot says how far behind the room its audio
// is running.
const lagReportEvery = 10 * time.Second

// How long a lost room follower waits before trying again, and how far the
// wait grows when failures repeat.
//
// Both ends are short on purpose: the server's socket grace is ten seconds, so
// a follower that comes back inside it never leaves the room at all. A ten
// second first wait meant the member was removed before the retry, which is the
// bot dropping out of the listening board and hopping back in later. The
// ceiling was thirty, which did the same thing every time failures repeated:
// past the grace the member is removed, and a member who leaves takes their
// queue with them - so the bot came back to a room whose queue had lost every
// song the unlinked users had asked for.
const (
	participantRetryFloor   = time.Second
	participantRetryCeiling = 5 * time.Second
)

// watchVoiceState is called for every voice state change in a guild. A channel
// the bot is in that nobody is left in is a channel to leave - music for an
// empty room is a bot sitting in a call on its own - and the grace covers the
// moment between somebody moving channels and their state update landing.
func (b *Bot) watchVoiceState(guildID string) {
	g := b.guild(guildID)
	g.mu.Lock()
	channelID := g.voiceChannel
	run := g.run
	if channelID == "" || run == nil {
		g.mu.Unlock()
		return
	}
	if b.channelHasListeners(guildID, channelID) {
		if g.emptyTimer != nil {
			g.emptyTimer.Stop()
			g.emptyTimer = nil
		}
		g.mu.Unlock()
		return
	}
	if g.emptyTimer != nil {
		g.mu.Unlock()
		return // already counting down
	}
	g.emptyTimer = time.AfterFunc(emptyChannelGrace, func() {
		g.mu.Lock()
		g.emptyTimer = nil
		channel := g.voiceChannel
		g.mu.Unlock()
		if channel != channelID || b.channelHasListeners(guildID, channel) {
			return
		}
		b.logger.Info("bot: the voice channel emptied; leaving", "channel", channel)
		b.leaveVoice(b.ctx, guildID)
	})
	g.mu.Unlock()
}

// emptyChannelGrace is how long a channel has to stay empty before the bot
// leaves it.
const emptyChannelGrace = 10 * time.Second

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

	// The lookup runs off the event loop: a blocked read is a socket the
	// server drops, and the database is shared with the rest of the bot.
	go func(itemID string) {
		owners, err := t.store.QueuedInRoom(context.Background(), t.roomID)
		if err != nil {
			t.logger.Warn("tracker: queue attribution", "error", err)
			return
		}
		t.mu.Lock()
		if t.current != nil && t.current.itemID == itemID {
			t.current.discordID = owners[itemID]
		}
		t.mu.Unlock()
	}(current.Item.ID)
	t.current = &playedSong{
		itemID:     current.Item.ID,
		trackID:    current.Item.TrackID,
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
	// Both writes are off the event loop, like the lookup above.
	go func(song playedSong, ms int64) {
		if err := t.store.RecordPlay(context.Background(), song.discordID, song.trackID, ms, now.UnixMilli()); err != nil {
			t.logger.Warn("tracker: record play", "error", err)
		}
		// The song has been heard, so it is not something a re-join should put
		// back into the queue.
		if err := t.store.ForgetQueued(context.Background(), t.roomID, song.itemID); err != nil {
			t.logger.Warn("tracker: forget a played queue item", "error", err)
		}
	}(*song, ms)
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
