// Package config loads the bot's configuration: a strict JSON file in the
// shape musoak's own configs take, with unknown keys rejected so a typo can
// never be silently ignored.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Account is the placeholder musoak account: the room member named
// "Discord Bot" that plays the room's mixed queue and holds every unlinked
// user's queue items, playlists, favorites and plays.
type Account struct {
	Username string `json:"username"`
	Password string `json:"password"`
	// DisplayName is the name the room shows for the placeholder's queue
	// items; the bot pins it on the account at startup.
	DisplayName string `json:"displayName,omitempty"`
}

// Config is the whole configuration.
type Config struct {
	// Token is the Discord bot token. The MUSOAK_DISCORD_TOKEN environment
	// variable wins over the file, so a deployment can keep the secret out of
	// the config entirely.
	Token string `json:"token"`
	// ServerURL is the musoak server this bot talks to.
	ServerURL string `json:"serverURL"`
	// Guilds are the Discord servers the bot serves; commands are registered
	// guild-scoped for each, which takes effect immediately. Empty means
	// global registration (Discord propagates it within the hour).
	Guilds []string `json:"guilds"`
	// Account is the placeholder musoak account.
	Account Account `json:"account"`
	// State is the sqlite file for links, ownership and room mappings.
	State string `json:"state"`
	// CacheDir is where the media cache keeps the room's songs.
	CacheDir string `json:"cacheDir"`
}

// Default fills the paths that have a conventional home, mirroring musoak's
// own config: XDG directories, falling back to the home directory.
func Default() *Config {
	return &Config{
		Account:  Account{DisplayName: "Discord Bot"},
		State:    filepath.Join(dataDir(), "musoak-discord-bot", "state.db"),
		CacheDir: filepath.Join(cacheDir(), "musoak-discord-bot"),
	}
}

// ResolvePath picks the config file the way musoak's CLI does: an explicit
// flag, then the environment, then the XDG default.
func ResolvePath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if env := os.Getenv("MUSOAK_DISCORD_BOT_CONFIG"); env != "" {
		return env
	}
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "musoak-discord-bot", "config.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "musoak-discord-bot", "config.json")
}

// Load reads the config at path. An absent file at the default path is not an
// error: the defaults (plus a token from the environment) are a working
// configuration for a bot that only needs a token and a server.
func Load(path string) (*Config, error) {
	explicit := path != ""
	if !explicit {
		path = ResolvePath("")
		if path == "" {
			return nil, errors.New("cannot determine config directory; pass --config")
		}
	}

	cfg := Default()
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) && !explicit {
			// A bot with no config has nothing to run on, so the first start
			// writes a starter one and says exactly where it is and how to
			// fill it - the token may also come from the environment instead.
			if writeErr := writeStarter(path); writeErr != nil {
				return nil, fmt.Errorf("open config: %w", writeErr)
			}
			return nil, fmt.Errorf("wrote a starter config to %s — put your Discord bot token in its \"token\" field (or set MUSOAK_DISCORD_TOKEN instead), fill in the MusOak server and the placeholder account, then start again", path)
		}
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("parse config %s: trailing data after JSON value", path)
	}

	// An explicitly blanked string falls back to the default; JSON cannot
	// distinguish "absent" from "empty" when decoding into filled defaults.
	if cfg.State == "" {
		cfg.State = Default().State
	}
	if cfg.CacheDir == "" {
		cfg.CacheDir = Default().CacheDir
	}
	if cfg.Account.DisplayName == "" {
		// An explicitly blanked name means the default, JSON cannot tell
		// absent from empty.
		cfg.Account.DisplayName = Default().Account.DisplayName
	}
	applyEnv(cfg)

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// writeStarter lays down a config to fill in: owner-only, because the token
// it is waiting for will live there.
func writeStarter(path string) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	starter := Config{
		Account: Account{DisplayName: "Discord Bot"},
	}
	body, err := json.MarshalIndent(starter, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(body, '\n'), 0o600)
}

func applyEnv(cfg *Config) {
	if token := os.Getenv("MUSOAK_DISCORD_TOKEN"); token != "" {
		cfg.Token = token
	}
}

// Validate reports what would stop the bot from running.
func (c *Config) Validate() error {
	if c.Token == "" {
		return errors.New("discord token is required (config \"token\" or MUSOAK_DISCORD_TOKEN)")
	}
	if c.ServerURL == "" {
		return errors.New("musoak server URL is required (config \"serverURL\")")
	}
	if c.Account.Username == "" {
		return errors.New("placeholder account is required (config \"account.username\")")
	}
	if c.Account.Password == "" {
		return errors.New("placeholder account is required (config \"account.password\")")
	}
	return nil
}

func dataDir() string {
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, ".local", "share")
}

func cacheDir() string {
	if dir := os.Getenv("XDG_CACHE_HOME"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, ".cache")
}
