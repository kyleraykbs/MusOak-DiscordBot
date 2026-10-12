package bot

import "github.com/bwmarrin/discordgo"

// commandDefs is the whole surface, registered guild by guild for instant
// effect (global registration would take up to an hour to appear).
func commandDefs() []*discordgo.ApplicationCommand {
	str := func(name, description string, required bool) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{
			Type:        discordgo.ApplicationCommandOptionString,
			Name:        name,
			Description: description,
			Required:    required,
		}
	}
	return []*discordgo.ApplicationCommand{
		{
			Name:        "join",
			Description: "Join a voice channel and start its MusOak room",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:         discordgo.ApplicationCommandOptionChannel,
					Name:         "channel",
					Description:  "The voice channel (defaults to yours)",
					ChannelTypes: []discordgo.ChannelType{discordgo.ChannelTypeGuildVoice},
				},
			},
		},
		{Name: "leave", Description: "Leave the voice channel; the room keeps its queue"},
		{
			Name:        "play",
			Description: "Search for a song and queue it for you",
			Options:     []*discordgo.ApplicationCommandOption{str("query", "What to search for", true)},
		},
		{
			Name:        "search",
			Description: "Search the library and the providers",
			Options:     []*discordgo.ApplicationCommandOption{str("query", "What to search for", true)},
		},
		{Name: "playlists", Description: "List your playlists"},
		{
			Name:        "playlist",
			Description: "Queue one of your playlists",
			Options:     []*discordgo.ApplicationCommandOption{str("name", "The playlist's name", true)},
		},
		{Name: "queue", Description: "What the room will play next"},
		{Name: "now", Description: "The room's current song"},
		{Name: "skip", Description: "Advance the room past this song"},
		{Name: "pause", Description: "Pause the room"},
		{Name: "resume", Description: "Resume the room"},
		{
			Name:        "seek",
			Description: "Jump to a position in the current song",
			Options:     []*discordgo.ApplicationCommandOption{str("time", "m:ss", true)},
		},
		{
			Name:        "vote",
			Description: "Score the current song, 1 (bad) to 5 (great)",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionInteger,
					Name:        "score",
					Description: "1 to 5",
					Required:    true,
					Choices: []*discordgo.ApplicationCommandOptionChoice{
						{Name: "1", Value: int64(1)},
						{Name: "2", Value: int64(2)},
						{Name: "3", Value: int64(3)},
						{Name: "4", Value: int64(4)},
						{Name: "5", Value: int64(5)},
					},
				},
			},
		},
		{Name: "favorite", Description: "Favorite the song the room is on"},
		{Name: "favorites", Description: "List your favorites"},
		{Name: "link", Description: "Link your MusOak account (opens a form)"},
		{Name: "unlink", Description: "Disconnect your MusOak account"},
		{Name: "whoami", Description: "Whether your MusOak account is linked"},
	}
}

// optionValue reads one option's value as a string, "" when absent.
func optionValue(data discordgo.ApplicationCommandInteractionData, name string) string {
	for _, opt := range data.Options {
		if opt.Name == name {
			return opt.StringValue()
		}
	}
	return ""
}
