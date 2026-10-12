package bot

import (
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestVoiceEventsMapForTheVoiceLayer(t *testing.T) {
	state := voiceStateEvent(&discordgo.VoiceStateUpdate{
		VoiceState: &discordgo.VoiceState{
			GuildID:   "1341810581988048929",
			ChannelID: "1514564335521894494",
			UserID:    "1341810581988048930",
			SessionID: "abc123",
			SelfMute:  true,
		},
	})
	if state.GuildID != 1341810581988048929 {
		t.Errorf("guild = %d", state.GuildID)
	}
	if state.ChannelID == nil || *state.ChannelID != 1514564335521894494 {
		t.Errorf("channel = %v, want the one from the event", state.ChannelID)
	}
	if state.UserID != 1341810581988048930 || state.SessionID != "abc123" || !state.SelfMute {
		t.Errorf("state = %+v", state.VoiceState)
	}

	// A disconnect carries no channel, and that nil is what the voice layer
	// reads as "close".
	left := voiceStateEvent(&discordgo.VoiceStateUpdate{
		VoiceState: &discordgo.VoiceState{GuildID: "1", UserID: "2", SessionID: "s"},
	})
	if left.ChannelID != nil {
		t.Errorf("left channel = %v, want nil", *left.ChannelID)
	}

	server := voiceServerEvent(&discordgo.VoiceServerUpdate{
		GuildID:  "1341810581988048929",
		Endpoint: "c-sjc05-8dcd4bfc.discord.media:2087",
		Token:    "tok",
	})
	if server.Endpoint == nil || *server.Endpoint != "c-sjc05-8dcd4bfc.discord.media:2087" {
		t.Errorf("endpoint = %v", server.Endpoint)
	}
	if server.Token != "tok" || server.GuildID != 1341810581988048929 {
		t.Errorf("server update = %+v", server)
	}
	empty := voiceServerEvent(&discordgo.VoiceServerUpdate{GuildID: "1"})
	if empty.Endpoint != nil {
		t.Errorf("an endpointless update gave %v", *empty.Endpoint)
	}
}

func TestSnowflakeIDReadsOrZeroes(t *testing.T) {
	if got := snowflakeID("1341810581988048929"); got != 1341810581988048929 {
		t.Errorf("parsed = %d", got)
	}
	if got := snowflakeID("not-an-id"); got != 0 {
		t.Errorf("unreadable = %d, want zero", got)
	}
}
