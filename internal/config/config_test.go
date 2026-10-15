package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTheFirstRunWritesAStarterConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("MUSOAK_DISCORD_BOT_CONFIG", "")
	t.Setenv("MUSOAK_DISCORD_TOKEN", "")
	path := filepath.Join(dir, "musoak-discord-bot", "config.json")

	_, err := Load("")
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("first Load = %v, want an error naming %s", err, path)
	}
	info, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatalf("no starter config: %v", statErr)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("starter config mode = %o, want 600 (the token goes in it)", perm)
	}

	// The starter parses as this bot's own config, and the second start says
	// what is missing rather than writing anything again.
	_, err = Load(path)
	if err == nil || !strings.Contains(err.Error(), "MUSOAK_DISCORD_TOKEN") {
		t.Fatalf("second Load = %v, want the token to be what is missing", err)
	}
}

func TestAnExplicitMissingConfigIsNotCreated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nowhere", "config.json")
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "open config") {
		t.Fatalf("Load(explicit missing) = %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("an explicit path was written anyway: %v", statErr)
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	path := writeConfig(t, `{
		"token": "x", "serverURL": "http://s", "account": {"username": "b", "password": "p"},
		"typoKey": true
	}`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "typoKey") {
		t.Fatalf("unknown key accepted: %v", err)
	}
}

func TestLoadRejectsTrailingData(t *testing.T) {
	path := writeConfig(t, `{"token":"x","serverURL":"http://s","account":{"username":"b","password":"p"}} {}`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "trailing data") {
		t.Fatalf("trailing data accepted: %v", err)
	}
}

func TestLoadFillsBlankedPathsWithDefaults(t *testing.T) {
	path := writeConfig(t, `{"token":"x","serverURL":"http://s","account":{"username":"b","password":"p"},"state":"","cacheDir":""}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.State != Default().State {
		t.Errorf("state = %q, want the default %q", cfg.State, Default().State)
	}
	if cfg.CacheDir != Default().CacheDir {
		t.Errorf("cacheDir = %q, want the default %q", cfg.CacheDir, Default().CacheDir)
	}
}

func TestEnvironmentTokenWinsOverFile(t *testing.T) {
	path := writeConfig(t, `{"token":"from-file","serverURL":"http://s","account":{"username":"b","password":"p"}}`)
	t.Setenv("MUSOAK_DISCORD_TOKEN", "from-env")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "from-env" {
		t.Errorf("token = %q, want the environment's", cfg.Token)
	}
}

func TestTheBufferAndVolumeDefaults(t *testing.T) {
	if got := Default().AudioBufferMs; got != DefaultAudioBufferMs {
		t.Errorf("default buffer = %d, want %d", got, DefaultAudioBufferMs)
	}
	if got := Default().AudioVolume; got != DefaultVolume {
		t.Errorf("default volume = %v, want %v", got, DefaultVolume)
	}
}

func TestTheVolumeDefault(t *testing.T) {
	if got := Default().AudioVolume; got != DefaultVolume {
		t.Errorf("default volume = %v, want %v", got, DefaultVolume)
	}
	path := writeConfig(t, `{"token":"x","serverURL":"http://s","account":{"username":"b","password":"p"},"audioVolume":0}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AudioVolume != DefaultVolume {
		t.Errorf("volume = %v, want the default", cfg.AudioVolume)
	}
	path = writeConfig(t, `{"token":"x","serverURL":"http://s","account":{"username":"b","password":"p"},"audioVolume":0.4}`)
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AudioVolume != 0.4 {
		t.Errorf("volume = %v, want 0.4 as configured", cfg.AudioVolume)
	}
}

func TestThePlaceholderNameDefaultsAndSurvivesBlank(t *testing.T) {
	if got := Default().Account.DisplayName; got != "Discord Bot" {
		t.Errorf("default displayName = %q", got)
	}
	path := writeConfig(t, `{"token":"x","serverURL":"http://s","account":{"username":"b","password":"p","displayName":""}}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Account.DisplayName != "Discord Bot" {
		t.Errorf("blanked displayName = %q, want the default", cfg.Account.DisplayName)
	}
}

func TestThePasswordCanComeFromTheEnvironment(t *testing.T) {
	// A config whose password is blank is valid when the environment carries
	// it: that is how a deployment keeps it out of the file.
	path := writeConfig(t, `{"token":"x","serverURL":"http://s","account":{"username":"b"}}`)
	if _, err := Load(path); err == nil {
		t.Fatal("a blank password was accepted without the environment")
	}
	t.Setenv("MUSOAK_DISCORD_PASSWORD", "from-env")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load with the password in the environment: %v", err)
	}
	if cfg.Account.Password != "from-env" {
		t.Errorf("password = %q, want the environment's", cfg.Account.Password)
	}
}

func TestValidateNamesWhatIsMissing(t *testing.T) {
	cases := []struct {
		mutate func(*Config)
		want   string
	}{
		{func(c *Config) { c.Token = "" }, "token"},
		{func(c *Config) { c.ServerURL = "" }, "serverURL"},
		{func(c *Config) { c.Account.Username = "" }, "account.username"},
		{func(c *Config) { c.Account.Password = "" }, "account.password"},
	}
	for _, tc := range cases {
		cfg := Default()
		cfg.Token, cfg.ServerURL = "t", "http://s"
		cfg.Account = Account{Username: "b", Password: "p"}
		tc.mutate(cfg)
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Validate() = %v, want an error naming %q", err, tc.want)
		}
	}
	cfg := Default()
	cfg.Token, cfg.ServerURL = "t", "http://s"
	cfg.Account = Account{Username: "b", Password: "p"}
	if err := cfg.Validate(); err != nil {
		t.Errorf("a complete config is invalid: %v", err)
	}
}

// A config this bot wrote itself carries the bot's old defaults, and those are
// not a decision anybody made: they are adopted as the new ones. A number
// somebody actually chose is kept exactly as it is.
func TestLegacyDefaultsAreMigratedAndChoicesAreKept(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "legacy.json")
	if err := os.WriteFile(legacy, []byte(`{"token":"x","serverURL":"http://s","account":{"username":"b","password":"p"},"audioVolume":0.5,"audioBufferMs":1500}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(legacy)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AudioVolume != DefaultVolume {
		t.Errorf("audioVolume = %v, want the new default %v", cfg.AudioVolume, DefaultVolume)
	}
	if cfg.AudioBufferMs != DefaultAudioBufferMs {
		t.Errorf("audioBufferMs = %d, want the new default %d", cfg.AudioBufferMs, DefaultAudioBufferMs)
	}
	if DefaultAudioBufferMs != 2000 {
		t.Errorf("the buffer default = %d, want two seconds of frames", DefaultAudioBufferMs)
	}

	chosen := filepath.Join(dir, "chosen.json")
	if err := os.WriteFile(chosen, []byte(`{"token":"x","serverURL":"http://s","account":{"username":"b","password":"p"},"audioVolume":0.8,"audioBufferMs":4000}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(chosen)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AudioVolume != 0.8 || cfg.AudioBufferMs != 4000 {
		t.Errorf("chosen values = %v/%d, want them left alone", cfg.AudioVolume, cfg.AudioBufferMs)
	}
}
