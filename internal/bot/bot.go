// Package bot is the Discord half: slash commands, the per-guild voice
// runtime, and the interaction plumbing that ties a Discord user to the
// musoak SDK.
package bot

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/bwmarrin/discordgo"

	"github.com/kyleraykbs/MusOak/pkg/client"

	"github.com/kyleraykbs/MusOak-DiscordBot/internal/config"
	"github.com/kyleraykbs/MusOak-DiscordBot/internal/state"
)

// Bot is the running service.
type Bot struct {
	cfg      *config.Config
	logger   *slog.Logger
	store    *state.Store
	accounts *accounts
	cache    *client.Cache
	session  *discordgo.Session
	voice    *voiceHub

	// ctx is the service lifetime; participants are children of it.
	ctx    context.Context
	cancel context.CancelFunc
	appID  string

	mu     sync.Mutex
	guilds map[string]*guildState
}

// New builds the bot: session, handlers, accounts. Nothing connects yet.
func New(cfg *config.Config, store *state.Store, cache *client.Cache, logger *slog.Logger) (*Bot, error) {
	if logger == nil {
		logger = slog.Default()
	}
	session, err := discordgo.New("Bot " + cfg.Token)
	if err != nil {
		return nil, fmt.Errorf("discord session: %w", err)
	}
	// Slash commands and voice states only: no privileged intents, so the
	// developer portal needs no toggles.
	session.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildVoiceStates

	ctx, cancel := context.WithCancel(context.Background())
	b := &Bot{
		cfg:      cfg,
		logger:   logger,
		store:    store,
		accounts: newAccounts(cfg, store, logger),
		cache:    cache,
		session:  session,
		ctx:      ctx,
		cancel:   cancel,
		guilds:   map[string]*guildState{},
	}
	b.voice = newVoiceHub(session, logger)
	session.AddHandler(func(s *discordgo.Session, update *discordgo.VoiceStateUpdate) {
		b.voice.handleState(update)
		// Anybody's state, not just ours: a channel the bot is in that
		// everybody left is a channel to leave.
		b.watchVoiceState(update.GuildID)
	})
	session.AddHandler(func(s *discordgo.Session, update *discordgo.VoiceServerUpdate) {
		b.voice.handleServer(update)
	})
	session.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		// Discord's dispatch is synchronous: the slow part always moves to its
		// own goroutine so one command can never hold the gateway.
		go b.onInteraction(i)
	})
	session.AddHandler(func(s *discordgo.Session, g *discordgo.GuildCreate) {
		// Invited while running: with no configured list the commands belong
		// in every server the bot is in, so they arrive with it.
		if len(b.cfg.Guilds) > 0 {
			return
		}
		go b.registerGuild(g.ID)
	})
	return b, nil
}

// Open connects to Discord, pins the placeholder's display name and registers
// the commands.
func (b *Bot) Open() error {
	ctx := b.ctx
	if _, err := b.accounts.login(ctx); err != nil {
		return err
	}
	if err := b.accounts.setDisplayName(ctx); err != nil {
		b.logger.Warn("bot: could not pin the placeholder's display name", "error", err)
	}
	if err := b.session.Open(); err != nil {
		return fmt.Errorf("discord connect: %w", err)
	}
	app, err := b.session.Application("@me")
	if err != nil {
		return fmt.Errorf("read the application: %w", err)
	}
	b.mu.Lock()
	b.appID = app.ID
	b.mu.Unlock()
	// A bot's user id is its application id, which is what the voice manager
	// needs to recognise its own voice states.
	if err := b.voice.start(app.ID); err != nil {
		return err
	}

	// Guild-scoped commands appear the moment they are registered, where a
	// global set can take an hour. With no list configured that means every
	// server the bot is already in, and servers it joins later register their
	// own on arrival (see the GuildCreate handler in New).
	guildIDs := b.cfg.Guilds
	if len(guildIDs) == 0 {
		for _, guild := range b.session.State.Guilds {
			guildIDs = append(guildIDs, guild.ID)
		}
	}
	if len(guildIDs) == 0 {
		b.logger.Info("bot: no servers yet — invite it and the commands register on arrival")
		return nil
	}
	for _, guildID := range guildIDs {
		if err := b.registerGuild(guildID); err != nil {
			return err
		}
	}
	b.logger.Info("bot: commands registered", "guilds", len(guildIDs), "count", len(commandDefs()))

	// A restart used to take the music with it. The room is remembered per
	// voice channel, so a channel that still has listeners gets its bot back
	// without anybody typing /join.
	go b.rejoinMappedChannels()
	return nil
}

// rejoinMappedChannels puts the bot back into every remembered voice channel
// that still has somebody in it.
func (b *Bot) rejoinMappedChannels() {
	ctx := b.ctx
	mappings, err := b.store.Rooms(ctx)
	if err != nil {
		b.logger.Warn("bot: could not read the room mappings", "error", err)
		return
	}
	for _, mapping := range mappings {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if !b.channelHasListeners(mapping.GuildID, mapping.ChannelID) {
			continue
		}
		if _, err := b.startGuild(ctx, mapping.GuildID, mapping.ChannelID); err != nil {
			b.logger.Warn("bot: could not return to a voice channel",
				"guild", mapping.GuildID, "channel", mapping.ChannelID, "error", err)
			continue
		}
		b.logger.Info("bot: returned to a voice channel after a restart",
			"guild", mapping.GuildID, "channel", mapping.ChannelID)
	}
}

// channelHasListeners reports whether anybody is in the channel to hear it:
// people, not other bots - a channel holding two music bots is an empty one.
func (b *Bot) channelHasListeners(guildID, channelID string) bool {
	if b.session.State.User == nil {
		return false
	}
	guild, err := b.session.State.Guild(guildID)
	if err != nil || guild == nil {
		return false
	}
	for _, state := range guild.VoiceStates {
		if state.ChannelID != channelID || state.UserID == b.session.State.User.ID {
			continue
		}
		if state.Member != nil && state.Member.User != nil && state.Member.User.Bot {
			continue
		}
		return true
	}
	return false
}

// registerGuild puts the whole command set into one server, replacing whatever
// was there so a renamed command cannot linger.
func (b *Bot) registerGuild(guildID string) error {
	b.mu.Lock()
	appID := b.appID
	b.mu.Unlock()
	if appID == "" {
		return nil // not connected yet; Open registers everything
	}
	if _, err := b.session.ApplicationCommandBulkOverwrite(appID, guildID, commandDefs()); err != nil {
		b.logger.Warn("bot: could not register commands", "guild", guildID, "error", err)
		return nil
	}
	b.logger.Info("bot: commands registered in a server", "guild", guildID)
	return nil
}

// Close ends every participant, the service lifetime and the gateway.
func (b *Bot) Close() {
	b.stopAll()
	b.cancel()
	_ = b.session.Close()
}
