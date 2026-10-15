// Package config loads the bot's configuration: a strict JSON file in the
// shape musoak's own configs take, with unknown keys rejected so a typo can
// never be silently ignored.
package config

import (
	"log/slog"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// DefaultAudioBufferMs is how much audio is queued ahead of the voice
// connection when the config does not say.
//
// It is a straight trade: the buffer is how much of a stall the bot can sit
// through without a gap, and it is also exactly how far behind the room its
// audio runs — the frames wait in the queue, so what leaves the wire is that
// much older. Measured play needed well under a second; a stuttering uplink
// wants more and pays for it in lag. A second and a half is the compromise,
// and audioBufferMs moves it either way. Two seconds: a stuttering uplink is
// the case this exists for, and the lag it costs is the price of hearing the
// song instead of the gaps between its packets.
const DefaultAudioBufferMs = 2000

// DefaultVolume is what the bot plays at unless the config says otherwise.
//
// A quarter: Discord's opus is not where headroom is found, and music arrives
// mastered hot, so the peaks heard at half were the mix reaching the ceiling.
// A config that sets audioVolume keeps its own number - deleting the key from
// an existing config.json is what adopts this one.
const DefaultVolume = 0.25

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
	// Guilds are the Discord servers the bot serves. Empty means every server
	// it is in, registered the moment it is invited.
	Guilds []string `json:"guilds"`
	// Account is the placeholder musoak account.
	Account Account `json:"account"`
	// State is the sqlite file for links, ownership and room mappings.
	State string `json:"state"`
	// CacheDir is where the media cache keeps the room's songs.
	CacheDir string `json:"cacheDir"`
	// AudioBufferMs is how much audio the bot keeps queued ahead of the voice
	// connection. It absorbs the hiccups — an ffmpeg relaunch between songs, a
	// scheduling blip, a stuttering uplink — that would otherwise starve the
	// connection's 20ms send loop and be heard as a drop. Default 2000; the
	// useful range is about 500 (snappy pauses) to 5000 (a bad link). The bot
	// plays that far behind the room, and the follower's drift tolerance
	// follows this number so the buffer is never mistaken for drift.
	AudioBufferMs int `json:"audioBufferMs,omitempty"`
	// AudioVolume is what the bot plays at, as a fraction of each file's own
	// level. Zero means the default (0.25): music is mastered hot and Discord's
	// opus has no headroom to give back.
	AudioVolume float64 `json:"audioVolume,omitempty"`
	// AudioBitrateKbps overrides the encoder's bitrate. Zero follows the voice
	// channel's own setting, which is what Discord caps the audio at — a
	// boosted channel is where a higher number has any effect.
	AudioBitrateKbps int `json:"audioBitrateKbps,omitempty"`
}

// The defaults this bot itself used to write into config files, before it knew
// better. A file still carrying one of these did not choose it - the bot did, on
// the day it made the file - so the current default is adopted instead, and the
// migration is said out loud rather than left for somebody to find. Any other
// value is a choice and is kept exactly as it is.
const (
	legacyVolume   = 0.5
	legacyBufferMs = 1500
)

// Default fills the paths that have a conventional home, mirroring musoak's
// own config: XDG directories, falling back to the home directory.
func Default() *Config {
	return &Config{
		Account:       Account{DisplayName: "Discord Bot"},
		AudioBufferMs: DefaultAudioBufferMs,
		AudioVolume:   DefaultVolume,
		State:         filepath.Join(dataDir(), "musoak-discord-bot", "state.db"),
		CacheDir:      filepath.Join(cacheDir(), "musoak-discord-bot"),
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
	if cfg.AudioBufferMs == 0 {
		cfg.AudioBufferMs = DefaultAudioBufferMs
	}
	if cfg.AudioVolume == 0 {
		cfg.AudioVolume = DefaultVolume
	}
	// A value that is exactly an old default is a default, not a decision: the
	// files this bot wrote by itself are full of them, and the bot's own idea of
	// loudness and buffering has moved since. Anything else is kept.
	if cfg.AudioVolume == legacyVolume {
		slog.Info("config: audioVolume was the old default", "was", legacyVolume, "now", DefaultVolume)
		cfg.AudioVolume = DefaultVolume
	}
	if cfg.AudioBufferMs == legacyBufferMs {
		slog.Info("config: audioBufferMs was the old default", "was", legacyBufferMs, "now", DefaultAudioBufferMs)
		cfg.AudioBufferMs = DefaultAudioBufferMs
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
	// The placeholder's password is a secret like the token, and a deployment
	// that keeps one out of the file keeps the other out too.
	if password := os.Getenv("MUSOAK_DISCORD_PASSWORD"); password != "" {
		cfg.Account.Password = password
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
