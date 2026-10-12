// Command musoak-discord-bot runs the MusOak Discord bot: rooms per voice
// channel, songs queued by linked and unlinked people alike, and the audio
// itself.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"codeberg.org/kyleraykbs/musoak/pkg/client"

	"codeberg.org/kyleraykbs/musoak-discord-bot/internal/bot"
	"codeberg.org/kyleraykbs/musoak-discord-bot/internal/config"
	"codeberg.org/kyleraykbs/musoak-discord-bot/internal/state"
)

func main() {
	configPath := flag.String("config", "", "path to config file (default: $MUSOAK_DISCORD_BOT_CONFIG, else $XDG_CONFIG_HOME/musoak-discord-bot/config.json)")
	verbose := flag.Bool("verbose", false, "log the voice handshake and every room event (debug level)")
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := config.Load(config.ResolvePath(*configPath))
	if err != nil {
		logger.Error("invalid config", "error", err)
		os.Exit(1)
	}

	store, err := state.Open(cfg.State)
	if err != nil {
		logger.Error("open state", "error", err)
		os.Exit(1)
	}
	defer store.Close()

	cache, err := client.OpenCache(cfg.CacheDir)
	if err != nil {
		logger.Error("open the media cache", "error", err)
		os.Exit(1)
	}

	service, err := bot.New(cfg, store, cache, logger)
	if err != nil {
		logger.Error("startup failed", "error", err)
		os.Exit(1)
	}
	if err := service.Open(); err != nil {
		logger.Error("startup failed", "error", err)
		os.Exit(1)
	}
	logger.Info("bot running",
		"config", config.ResolvePath(*configPath),
		"server", cfg.ServerURL,
		"placeholder", cfg.Account.Username,
		"guilds", len(cfg.Guilds))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	logger.Info("shutting down")
	service.Close()
}
