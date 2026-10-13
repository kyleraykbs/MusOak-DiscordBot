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

// A channel with nobody to hear it is an empty one: the bot itself and other
// bots do not count, and somebody in a different channel is not a listener.
func TestAnEmptyChannelIsOneWithNoPeopleInIt(t *testing.T) {
	session, err := discordgo.New("Bot test")
	if err != nil {
		t.Fatal(err)
	}
	session.State.User = &discordgo.User{ID: "bot"}
	guild := &discordgo.Guild{ID: "g1", VoiceStates: []*discordgo.VoiceState{
		{GuildID: "g1", ChannelID: "c1", UserID: "bot"},
		{GuildID: "g1", ChannelID: "c1", UserID: "other-bot",
			Member: &discordgo.Member{User: &discordgo.User{ID: "other-bot", Bot: true}}},
	}}
	if err := session.State.GuildAdd(guild); err != nil {
		t.Fatal(err)
	}
	b := &Bot{session: session}
	if b.channelHasListeners("g1", "c1") {
		t.Error("the bot and another bot are nobody to play for")
	}

	guild.VoiceStates = append(guild.VoiceStates, &discordgo.VoiceState{
		GuildID: "g1", ChannelID: "c1", UserID: "kyle",
		Member: &discordgo.Member{User: &discordgo.User{ID: "kyle"}},
	})
	if !b.channelHasListeners("g1", "c1") {
		t.Error("a person in the channel was not counted")
	}

	guild.VoiceStates[2].ChannelID = "c2"
	if b.channelHasListeners("g1", "c1") {
		t.Error("somebody in another channel counted as a listener")
	}
}
