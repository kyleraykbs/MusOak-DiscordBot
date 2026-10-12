package bot

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/bwmarrin/discordgo"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	davesession "github.com/thomas-vilte/dave-go/session"
)

// voiceHub owns the voice side: one connection per guild, fed by the gateway
// events discordgo already delivers.
//
// discordgo keeps the gateway, the commands and the modals — everything that
// works. The audio leg is disgo's voice package with dave-go, because Discord
// has required the DAVE (E2EE) handshake on non-stage voice since March 2026
// and discordgo has never implemented it: its voice socket is closed with
// 4017 before a single frame is sent.
type voiceHub struct {
	session *discordgo.Session
	logger  *slog.Logger

	mu      sync.Mutex
	manager voice.Manager
	// dave is dave-go's own session, captured when the connection makes it:
	// it knows whether the group is mid-change, which is when frames have to
	// wait rather than go out under an epoch nobody is listening on yet.
	dave *davesession.Session
}

func newVoiceHub(session *discordgo.Session, logger *slog.Logger) *voiceHub {
	return &voiceHub{session: session, logger: logger}
}

// start builds the manager once the bot's own user id is known — for a bot
// that is its application id. dave-go is the DAVE session: pure Go, no cgo,
// so the binary stays static.
func (h *voiceHub) start(userID string) error {
	id, err := snowflake.Parse(userID)
	if err != nil {
		return fmt.Errorf("voice: bot user id %q: %w", userID, err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.manager = voice.NewManager(h.stateUpdate, id,
		voice.WithLogger(h.logger),
		voice.WithDaveSessionCreateFunc(davesession.CreateFunc(
			davesession.WithSessionHook(func(session *davesession.Session) {
				h.mu.Lock()
				h.dave = session
				h.mu.Unlock()
				h.logger.Debug("voice: dave session ready")
			}),
		)),
	)
	return nil
}

// shouldHoldFrames is what the sink asks before handing a frame over: true
// while the DAVE group is changing, so the frame waits in the buffer instead
// of being sent under an epoch the listeners have left.
func (h *voiceHub) shouldHoldFrames() bool {
	h.mu.Lock()
	session := h.dave
	h.mu.Unlock()
	if session == nil {
		return false
	}
	return session.ShouldHoldFrames()
}

// stateUpdate is what a voice connection asks for: send this voice state to
// the gateway. discordgo's manual join is exactly that — op 4 and nothing
// else — and a nil channel is the disconnect.
func (h *voiceHub) stateUpdate(ctx context.Context, guildID snowflake.ID, channelID *snowflake.ID, selfMute, selfDeaf bool) error {
	channel := ""
	if channelID != nil {
		channel = channelID.String()
	}
	return h.session.ChannelVoiceJoinManual(guildID.String(), channel, selfMute, selfDeaf)
}

// handleState forwards a voice state update — ours alone; other people's are
// the connection's business to ignore, and it does.
func (h *voiceHub) handleState(update *discordgo.VoiceStateUpdate) {
	h.mu.Lock()
	manager := h.manager
	h.mu.Unlock()
	if manager == nil || h.session.State.User == nil || update.UserID != h.session.State.User.ID {
		return
	}
	manager.HandleVoiceStateUpdate(voiceStateEvent(update))
}

// handleServer forwards a voice server update.
func (h *voiceHub) handleServer(update *discordgo.VoiceServerUpdate) {
	h.mu.Lock()
	manager := h.manager
	h.mu.Unlock()
	if manager == nil {
		return
	}
	manager.HandleVoiceServerUpdate(voiceServerEvent(update))
}

// open puts the guild's connection on a channel, creating the connection on
// first use. It returns once the voice gateway is up, which is what the sink
// needs before it streams anything.
func (h *voiceHub) open(ctx context.Context, guildID, channelID string) (voice.Conn, error) {
	h.mu.Lock()
	manager := h.manager
	h.mu.Unlock()
	if manager == nil {
		return nil, fmt.Errorf("voice: not connected to Discord yet")
	}
	guild, err := snowflake.Parse(guildID)
	if err != nil {
		return nil, fmt.Errorf("voice: guild %q: %w", guildID, err)
	}
	channel, err := snowflake.Parse(channelID)
	if err != nil {
		return nil, fmt.Errorf("voice: channel %q: %w", channelID, err)
	}
	conn := manager.CreateConn(guild)
	if current := conn.ChannelID(); current != nil && *current == channel {
		return conn, nil // already there
	}
	if err := conn.Open(ctx, channel, false, false); err != nil {
		return nil, fmt.Errorf("joining the voice channel: %w", err)
	}
	return conn, nil
}

// close leaves the guild's voice channel, if it is in one.
func (h *voiceHub) close(ctx context.Context, guildID string) {
	h.mu.Lock()
	manager := h.manager
	h.mu.Unlock()
	if manager == nil {
		return
	}
	guild, err := snowflake.Parse(guildID)
	if err != nil {
		return
	}
	if conn := manager.GetConn(guild); conn != nil {
		conn.Close(ctx)
	}
}

// voiceStateEvent maps discordgo's event onto disgo's.
func voiceStateEvent(update *discordgo.VoiceStateUpdate) gateway.EventVoiceStateUpdate {
	event := gateway.EventVoiceStateUpdate{
		VoiceState: discord.VoiceState{
			GuildID:   snowflakeID(update.GuildID),
			UserID:    snowflakeID(update.UserID),
			SessionID: update.SessionID,
			SelfMute:  update.SelfMute,
			SelfDeaf:  update.SelfDeaf,
		},
	}
	if update.ChannelID != "" {
		channel := snowflakeID(update.ChannelID)
		event.ChannelID = &channel
	}
	return event
}

// voiceServerEvent maps discordgo's event onto disgo's.
func voiceServerEvent(update *discordgo.VoiceServerUpdate) gateway.EventVoiceServerUpdate {
	event := gateway.EventVoiceServerUpdate{
		GuildID: snowflakeID(update.GuildID),
		Token:   update.Token,
	}
	if update.Endpoint != "" {
		endpoint := update.Endpoint
		event.Endpoint = &endpoint
	}
	return event
}

// snowflakeID reads an id Discord sent; anything unreadable is zero, which
// every consumer treats as "not this one".
func snowflakeID(raw string) snowflake.ID {
	id, err := snowflake.Parse(raw)
	if err != nil {
		return 0
	}
	return id
}
