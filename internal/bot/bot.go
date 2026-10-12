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

	"codeberg.org/kyleraykbs/musoak/pkg/client"

	"codeberg.org/kyleraykbs/musoak-discord-bot/internal/config"
	"codeberg.org/kyleraykbs/musoak-discord-bot/internal/state"
)

// Bot is the running service.
type Bot struct {
	cfg      *config.Config
	logger   *slog.Logger
	store    *state.Store
	accounts *accounts
	cache    *client.Cache
	session  *discordgo.Session

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
	return nil
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
