package bot

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"codeberg.org/kyleraykbs/musoak/pkg/client"

	"codeberg.org/kyleraykbs/musoak-discord-bot/internal/link"
	"codeberg.org/kyleraykbs/musoak-discord-bot/internal/state"
)

// ephemeralCommands are the ones nobody else needs to see: identity matters
// only to the person asking. The link answers with a form, not a message.
var ephemeralCommands = map[string]bool{
	"unlink": true,
	"whoami": true,
}

// onInteraction routes one interaction. It runs in its own goroutine — the
// gateway's dispatch is synchronous.
func (b *Bot) onInteraction(i *discordgo.InteractionCreate) {
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		data := i.ApplicationCommandData()
		if data.Name == "link" {
			b.respondLinkForm(i)
			return
		}
		if err := b.deferReply(i, ephemeralCommands[data.Name]); err != nil {
			b.logger.Warn("bot: could not acknowledge a command", "command", data.Name, "error", err)
			return
		}
		answer, err := b.dispatch(i, data)
		if err != nil {
			answer = "❌ " + err.Error()
		}
		if editErr := b.editReply(i, answer); editErr != nil {
			b.logger.Warn("bot: could not answer a command", "command", data.Name, "error", editErr)
		}

	case discordgo.InteractionModalSubmit:
		data := i.ModalSubmitData()
		if data.CustomID != "link-modal" {
			return
		}
		if err := b.deferReply(i, true); err != nil {
			return
		}
		answer := b.linkFromModal(i)
		if editErr := b.editReply(i, answer); editErr != nil {
			b.logger.Warn("bot: could not answer the link", "error", editErr)
		}
	}
}

// dispatch runs one command and returns what the room should read.
func (b *Bot) dispatch(i *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData) (string, error) {
	ctx := b.ctx
	switch data.Name {
	case "join":
		return b.cmdJoin(ctx, i, data)
	case "leave":
		b.leaveVoice(ctx, i.GuildID)
		return "Left the voice channel — the room keeps its queue.", nil
	case "play":
		return b.cmdPlay(ctx, i, data)
	case "search":
		return b.cmdSearch(ctx, i, data)
	case "playlists":
		return b.cmdPlaylists(ctx, i)
	case "playlist":
		return b.cmdPlaylist(ctx, i, data)
	case "queue":
		return b.cmdQueue(ctx, i)
	case "now":
		return b.cmdNow(ctx, i)
	case "skip":
		return b.roomControl(ctx, i, "skip")
	case "pause":
		return b.roomControl(ctx, i, "pause")
	case "resume":
		return b.roomControl(ctx, i, "resume")
	case "seek":
		return b.cmdSeek(ctx, i, data)
	case "vote":
		return b.cmdVote(ctx, i, data)
	case "favorite":
		return b.cmdFavorite(ctx, i)
	case "favorites":
		return b.cmdFavorites(ctx, i)
	case "unlink":
		return b.cmdUnlink(ctx, i)
	case "whoami":
		return b.cmdWhoami(ctx, i)
	}
	return "", fmt.Errorf("unknown command %q", data.Name)
}

// --- interaction plumbing --------------------------------------------------

func (b *Bot) deferReply(i *discordgo.InteractionCreate, ephemeral bool) error {
	resp := &discordgo.InteractionResponse{Type: discordgo.InteractionResponseDeferredChannelMessageWithSource}
	if ephemeral {
		resp.Data = &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral}
	}
	return b.session.InteractionRespond(i.Interaction, resp)
}

func (b *Bot) editReply(i *discordgo.InteractionCreate, content string) error {
	_, err := b.session.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
	return err
}

// respondLinkForm opens the credentials form. It is the one command that
// answers immediately: a modal is its own response.
func (b *Bot) respondLinkForm(i *discordgo.InteractionCreate) {
	resp := &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseModal,
		Data: &discordgo.InteractionResponseData{
			CustomID: "link-modal",
			Title:    "Link your MusOak account",
			Content:  "Your credentials go to the server you configured, nowhere else.",
			Components: []discordgo.MessageComponent{
				&discordgo.ActionsRow{Components: []discordgo.MessageComponent{
					&discordgo.TextInput{
						CustomID: "username", Label: "MusOak username",
						Style: discordgo.TextInputShort, Required: true,
					},
				}},
				&discordgo.ActionsRow{Components: []discordgo.MessageComponent{
					&discordgo.TextInput{
						CustomID: "password", Label: "MusOak password",
						Style: discordgo.TextInputShort, Required: true,
					},
				}},
			},
		},
	}
	if err := b.session.InteractionRespond(i.Interaction, resp); err != nil {
		b.logger.Warn("bot: could not open the link form", "error", err)
	}
}

func userID(i *discordgo.InteractionCreate) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	if i.User != nil {
		return i.User.ID
	}
	return ""
}

// --- commands --------------------------------------------------------------

// cmdJoin resolves the voice channel (the option, else the invoker's) and
// starts the bot there.
func (b *Bot) cmdJoin(ctx context.Context, i *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData) (string, error) {
	if i.GuildID == "" {
		return "", fmt.Errorf("this only works in a server")
	}
	channelID := optionRaw(data, "channel")
	if channelID == "" {
		vs, err := b.session.State.VoiceState(i.GuildID, userID(i))
		if err != nil || vs.ChannelID == "" {
			return "", fmt.Errorf("you are not in a voice channel — join one, or pass channel=")
		}
		channelID = vs.ChannelID
	}
	if _, err := b.startGuild(ctx, i.GuildID, channelID); err != nil {
		return "", err
	}
	name := channelID
	if channel, err := b.session.State.Channel(channelID); err == nil {
		name = "#" + channel.Name
	}
	return fmt.Sprintf("Playing in %s — queue something with /play.", name), nil
}

// cmdPlay auto-joins the invoker's channel, searches as them (or as the
// placeholder when unlinked) and queues the top hit for them.
func (b *Bot) cmdPlay(ctx context.Context, i *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData) (string, error) {
	roomID, picked, providerNotes, err := b.searchAndQueueRoom(ctx, i, data, 1)
	if err != nil {
		return "", err
	}
	track := picked[0]
	_, err = b.queueTracks(ctx, userID(i), roomID, []string{track.ID})
	if err != nil {
		return "", err
	}
	msg := fmt.Sprintf("Queued **%s**", track.Title)
	if artists := strings.Join(track.Artists, ", "); artists != "" {
		msg += " — " + artists
	}
	msg += fmt.Sprintf(" · %s", formatDuration(track.DurationMs))
	if providerNotes != "" {
		msg += "\n" + providerNotes
	}
	return msg, nil
}

// cmdSearch shows the top hits without queueing anything.
func (b *Bot) cmdSearch(ctx context.Context, i *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData) (string, error) {
	_, picked, providerNotes, err := b.searchAndQueueRoom(ctx, i, data, 5)
	if err != nil {
		return "", err
	}
	var lines []string
	for n, track := range picked {
		line := fmt.Sprintf("`%d.` **%s**", n+1, track.Title)
		if artists := strings.Join(track.Artists, ", "); artists != "" {
			line += " — " + artists
		}
		lines = append(lines, line+" · "+formatDuration(track.DurationMs))
	}
	msg := strings.Join(lines, "\n")
	msg += "\nUse `/play` with what you want; it takes the first hit."
	if providerNotes != "" {
		msg += "\n" + providerNotes
	}
	return msg, nil
}

// searchAndQueueRoom resolves the room the search results will go to (which
// auto-joins the invoker's voice channel) and searches as the requester.
func (b *Bot) searchAndQueueRoom(ctx context.Context, i *discordgo.InteractionCreate,
	data discordgo.ApplicationCommandInteractionData, want int) (string, []*client.Track, string, error) {

	query := strings.TrimSpace(optionValue(data, "query"))
	if query == "" {
		return "", nil, "", fmt.Errorf("say what to search for")
	}
	roomID, err := b.playRoom(ctx, i)
	if err != nil {
		return "", nil, "", err
	}

	var picked []*client.Track
	var providerNotes string
	_, err = b.accounts.requester(ctx, userID(i), func(c *client.Client) error {
		result, err := c.Search(ctx, query, 10)
		if err != nil {
			return err
		}
		for _, group := range result.Groups {
			if len(picked) >= want {
				break
			}
			track := group.Track
			picked = append(picked, &track)
		}
		if len(picked) == 0 {
			msg := fmt.Sprintf("nothing came back for %q", query)
			if len(result.ProviderErrors) > 0 {
				var failing []string
				for _, pe := range result.ProviderErrors {
					failing = append(failing, pe.Provider)
				}
				msg += " (" + strings.Join(failing, ", ") + " failed)"
			}
			return fmt.Errorf("%s", msg)
		}
		if len(result.ProviderErrors) > 0 {
			providerNotes = fmt.Sprintf("%d provider(s) failed to answer", len(result.ProviderErrors))
		}
		return nil
	})
	if err != nil {
		return "", nil, "", err
	}
	return roomID, picked, providerNotes, nil
}

// playRoom is the room a /play is for: the invoker's voice channel's room,
// starting the bot there — which is what moves it when somebody plays from a
// different channel.
func (b *Bot) playRoom(ctx context.Context, i *discordgo.InteractionCreate) (string, error) {
	if i.GuildID == "" {
		return "", fmt.Errorf("this only works in a server")
	}
	vs, err := b.session.State.VoiceState(i.GuildID, userID(i))
	if err != nil || vs.ChannelID == "" {
		return "", fmt.Errorf("join a voice channel first — that is where the music goes")
	}
	return b.startGuild(ctx, i.GuildID, vs.ChannelID)
}

// queueTracks puts the songs in the requester's own part of the room's queue:
// under their account when linked, under the placeholder (attributed to them
// in the store) when not.
func (b *Bot) queueTracks(ctx context.Context, discordID, roomID string, trackIDs []string) (int, error) {
	linked, err := b.accounts.withUser(ctx, discordID, func(c *client.Client) error {
		// Their own account: the items are theirs by membership, nothing to
		// remember.
		room, err := c.JoinRoom(ctx, roomID)
		if err != nil {
			return err
		}
		_, err = room.QueueMany(ctx, trackIDs)
		return err
	})
	if err != nil {
		return 0, err
	}
	if linked {
		return len(trackIDs), nil
	}

	// The placeholder holds these: remember which items were theirs so the
	// merge can move them later.
	added := 0
	err = b.accounts.withPlaceholder(ctx, func(c *client.Client) error {
		room, err := c.JoinRoom(ctx, roomID)
		if err != nil {
			return err
		}
		before, err := room.Snapshot(ctx)
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, item := range before.Queues[room.MemberID] {
			seen[item.ID] = true
		}
		if _, err := room.QueueMany(ctx, trackIDs); err != nil {
			return err
		}
		after, err := room.Snapshot(ctx)
		if err != nil {
			return err
		}
		for _, item := range after.Queues[room.MemberID] {
			if seen[item.ID] {
				continue
			}
			if err := b.store.RememberQueued(ctx, state.Queued{
				RoomID: roomID, ItemID: item.ID, DiscordID: discordID, TrackID: item.TrackID,
			}); err != nil {
				b.logger.Warn("bot: queue attribution", "error", err)
			}
			added++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if added == 0 {
		added = len(trackIDs) // the room answered without the diff agreeing; report the ask
	}
	return added, nil
}

func (b *Bot) cmdPlaylists(ctx context.Context, i *discordgo.InteractionCreate) (string, error) {
	var lines []string
	_, err := b.accounts.requester(ctx, userID(i), func(c *client.Client) error {
		playlists, err := c.Playlists(ctx)
		if err != nil {
			return err
		}
		owned, err := b.ownedSet(ctx, userID(i), "playlist")
		if err != nil {
			return err
		}
		for _, playlist := range playlists {
			if owned != nil && !owned[playlist.ID] {
				continue // somebody else's, seen through the placeholder
			}
			lines = append(lines, fmt.Sprintf("• **%s** — %d songs", playlist.Name, playlist.TrackCount))
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(lines) == 0 {
		return "No playlists yet — build one in the web UI, then /playlist it here.", nil
	}
	if len(lines) > 15 {
		lines = append(lines[:15], fmt.Sprintf("…and %d more", len(lines)-15))
	}
	return strings.Join(lines, "\n"), nil
}

func (b *Bot) cmdPlaylist(ctx context.Context, i *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData) (string, error) {
	name := strings.TrimSpace(optionValue(data, "name"))
	if name == "" {
		return "", fmt.Errorf("name the playlist")
	}
	roomID, err := b.playRoom(ctx, i)
	if err != nil {
		return "", err
	}

	var trackIDs []string
	var playlistName string
	_, err = b.accounts.requester(ctx, userID(i), func(c *client.Client) error {
		playlist, err := b.findPlaylist(ctx, c, userID(i), name)
		if err != nil {
			return err
		}
		detail, err := c.Playlist(ctx, playlist.ID)
		if err != nil {
			return err
		}
		playlistName = playlist.Name
		items := append([]client.PlaylistItem(nil), detail.Items...)
		sort.SliceStable(items, func(a, b int) bool { return items[a].Position < items[b].Position })
		for _, item := range items {
			trackIDs = append(trackIDs, item.Track.ID)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(trackIDs) == 0 {
		return "", fmt.Errorf("%q has no songs in it", name)
	}
	added, err := b.queueTracks(ctx, userID(i), roomID, trackIDs)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Queued %d song(s) from **%s**.", added, playlistName), nil
}

func (b *Bot) cmdQueue(ctx context.Context, i *discordgo.InteractionCreate) (string, error) {
	roomID, err := b.targetRoom(i.GuildID, userID(i))
	if err != nil {
		return "", err
	}
	room, err := b.placeholderRoom(ctx, roomID)
	if err != nil {
		return "", err
	}
	pending := room.Pending()
	if len(pending) == 0 {
		return "Nothing queued — /play something.", nil
	}
	var lines []string
	if room.Current != nil {
		lines = append(lines, fmt.Sprintf("**Now:** %s", room.Current.Item.Title))
	}
	show := pending
	if len(show) > 10 {
		show = show[:10]
	}
	for n, item := range show {
		lines = append(lines, fmt.Sprintf("`%d.` %s — queued by %s", n+1, item.Title, memberName(room, item.AddedBy)))
	}
	if rest := len(pending) - len(show); rest > 0 {
		lines = append(lines, fmt.Sprintf("…and %d more", rest))
	}
	return strings.Join(lines, "\n"), nil
}

func (b *Bot) cmdNow(ctx context.Context, i *discordgo.InteractionCreate) (string, error) {
	roomID, err := b.targetRoom(i.GuildID, userID(i))
	if err != nil {
		return "", err
	}
	room, err := b.placeholderRoom(ctx, roomID)
	if err != nil {
		return "", err
	}
	current := room.Current
	if current == nil {
		return "Nothing playing.", nil
	}
	line := fmt.Sprintf("**%s**", current.Item.Title)
	if artists := strings.Join(current.Item.Artists, ", "); artists != "" {
		line += " — " + artists
	}
	position := current.PositionAt(room.ServerNowMs)
	line += fmt.Sprintf("\n%s / %s", formatDuration(position), formatDuration(current.DurationMs))
	if current.Paused {
		line += " · paused"
	}
	if current.MeanScore > 0 {
		line += fmt.Sprintf("\naverage score %.1f", current.MeanScore)
	}
	return line, nil
}

// roomControl runs one of the placeholder's room commands: skip, pause, resume.
func (b *Bot) roomControl(ctx context.Context, i *discordgo.InteractionCreate, verb string) (string, error) {
	roomID, err := b.targetRoom(i.GuildID, userID(i))
	if err != nil {
		return "", err
	}
	var snapshot *client.Room
	err = b.accounts.withPlaceholder(ctx, func(c *client.Client) error {
		room, err := c.JoinRoom(ctx, roomID)
		if err != nil {
			return err
		}
		switch verb {
		case "skip":
			snapshot, err = room.Skip(ctx)
		case "pause":
			snapshot, err = room.Pause(ctx)
		case "resume":
			snapshot, err = room.Resume(ctx)
		}
		return err
	})
	if err != nil {
		return "", err
	}
	switch verb {
	case "skip":
		if snapshot != nil && snapshot.Current != nil {
			return fmt.Sprintf("Skipped to **%s**.", snapshot.Current.Item.Title), nil
		}
		return "Skipped — nothing is on now.", nil
	case "pause":
		return "Paused.", nil
	default:
		return "Resumed.", nil
	}
}

func (b *Bot) cmdSeek(ctx context.Context, i *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData) (string, error) {
	raw := strings.TrimSpace(optionValue(data, "time"))
	position, err := parseTimecode(raw)
	if err != nil {
		return "", err
	}
	roomID, err := b.targetRoom(i.GuildID, userID(i))
	if err != nil {
		return "", err
	}
	err = b.accounts.withPlaceholder(ctx, func(c *client.Client) error {
		room, err := c.JoinRoom(ctx, roomID)
		if err != nil {
			return err
		}
		_, err = room.Seek(ctx, position)
		return err
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Seeked to %s.", formatDuration(position)), nil
}

func (b *Bot) cmdVote(ctx context.Context, i *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData) (string, error) {
	score := int(optionInt(data, "score"))
	if score < 1 || score > 5 {
		return "", fmt.Errorf("score from 1 to 5")
	}
	roomID, err := b.targetRoom(i.GuildID, userID(i))
	if err != nil {
		return "", err
	}
	linked, err := b.accounts.withUser(ctx, userID(i), func(c *client.Client) error {
		room, err := c.JoinRoom(ctx, roomID)
		if err != nil {
			return err
		}
		_, err = room.Vote(ctx, score)
		return err
	})
	if err != nil {
		return "", err
	}
	if !linked {
		return "", link.ErrNotLinked
	}
	return fmt.Sprintf("Scored it %d/5.", score), nil
}

func (b *Bot) cmdFavorite(ctx context.Context, i *discordgo.InteractionCreate) (string, error) {
	roomID, err := b.targetRoom(i.GuildID, userID(i))
	if err != nil {
		return "", err
	}
	room, err := b.placeholderRoom(ctx, roomID)
	if err != nil {
		return "", err
	}
	if room.Current == nil {
		return "", fmt.Errorf("nothing is playing to favorite")
	}
	track := room.Current.Item
	_, err = b.accounts.requester(ctx, userID(i), func(c *client.Client) error {
		return c.AddFavorite(ctx, track.TrackID)
	})
	if err != nil {
		return "", err
	}
	// Attribute an unlinked user's favorite so the merge can move it.
	if err := b.attributeFavorite(ctx, userID(i), track.TrackID); err != nil {
		b.logger.Warn("bot: favorite attribution", "error", err)
	}
	return fmt.Sprintf("Favorited **%s**.", track.Title), nil
}

func (b *Bot) cmdFavorites(ctx context.Context, i *discordgo.InteractionCreate) (string, error) {
	var lines []string
	_, err := b.accounts.requester(ctx, userID(i), func(c *client.Client) error {
		favorites, err := c.Favorites(ctx)
		if err != nil {
			return err
		}
		owned, err := b.ownedSet(ctx, userID(i), "favorite")
		if err != nil {
			return err
		}
		for _, track := range favorites {
			if owned != nil && !owned[track.ID] {
				continue
			}
			line := "• **" + track.Title + "**"
			if artists := strings.Join(track.Artists, ", "); artists != "" {
				line += " — " + artists
			}
			lines = append(lines, line)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(lines) == 0 {
		return "No favorites yet — /favorite puts the current song here.", nil
	}
	if len(lines) > 15 {
		lines = append(lines[:15], fmt.Sprintf("…and %d more", len(lines)-15))
	}
	return strings.Join(lines, "\n"), nil
}

func (b *Bot) cmdWhoami(ctx context.Context, i *discordgo.InteractionCreate) (string, error) {
	link, ok, err := b.store.LinkOf(ctx, userID(i))
	if err != nil {
		return "", err
	}
	if !ok {
		return "Not linked. `/link` with your MusOak account and your playlists, favorites and queue come with you.", nil
	}
	return fmt.Sprintf("Linked as **%s** (since %s). `/unlink` disconnects; everything stays on your account.",
		link.Username, link.LinkedAt.Format("Jan 2")), nil
}

func (b *Bot) cmdUnlink(ctx context.Context, i *discordgo.InteractionCreate) (string, error) {
	link, ok, err := b.store.LinkOf(ctx, userID(i))
	if err != nil {
		return "", err
	}
	if !ok {
		return "Nothing to unlink — you are not linked.", nil
	}
	if err := b.store.Unlink(ctx, userID(i)); err != nil {
		return "", err
	}
	// Revoke the stored session; a failure here only leaves a token to expire.
	go func() {
		c := client.New(b.accounts.serverURL, client.WithToken(link.Token))
		if err := c.Logout(context.Background()); err != nil {
			b.logger.Debug("bot: logout of an unlinked account", "error", err)
		}
	}()
	return "Unlinked. Everything you built stays on your MusOak account.", nil
}

// linkFromModal logs the credentials in, records the link and merges the
// unlinked history onto the account.
func (b *Bot) linkFromModal(i *discordgo.InteractionCreate) string {
	ctx := b.ctx
	username, password := modalFields(i)
	if username == "" || password == "" {
		return "Both fields are needed."
	}
	auth, err := client.New(b.accounts.serverURL).Login(ctx, username, password)
	if err != nil {
		return "Could not sign in: " + err.Error()
	}
	if err := b.store.PutLink(ctx, linkFor(userID(i), username, password, auth.Token)); err != nil {
		return "Could not save the link: " + err.Error()
	}

	user := client.New(b.accounts.serverURL, client.WithToken(auth.Token))
	var sum link.Summary
	err = b.accounts.withPlaceholder(ctx, func(placeholder *client.Client) error {
		merger := &link.Merger{Placeholder: placeholder, Store: b.store, Logger: b.logger}
		var err error
		sum, err = merger.Merge(ctx, userID(i), user)
		return err
	})
	if err != nil {
		return fmt.Sprintf("Signed in as **%s**, but the merge failed: %s", username, err.Error())
	}
	return fmt.Sprintf("Linked as **%s**. %s", username, mergeReport(sum))
}

// mergeReport is the one-line summary of what a merge moved.
func mergeReport(sum link.Summary) string {
	var parts []string
	if sum.Playlists > 0 {
		parts = append(parts, fmt.Sprintf("%d playlist(s)", sum.Playlists))
	}
	if sum.Favorites > 0 {
		parts = append(parts, fmt.Sprintf("%d favorite(s)", sum.Favorites))
	}
	if sum.Plays > 0 {
		parts = append(parts, fmt.Sprintf("%d listen(s)", sum.Plays))
	}
	if sum.Queued > 0 {
		parts = append(parts, fmt.Sprintf("%d queued song(s)", sum.Queued))
	}
	if sum.Ranking {
		parts = append(parts, "your provider order")
	}
	report := "Nothing to move over."
	if len(parts) > 0 {
		report = "Moved over: " + strings.Join(parts, ", ") + "."
	}
	if len(sum.Notes) > 0 {
		notes := sum.Notes
		if len(notes) > 3 {
			notes = append(notes[:3], fmt.Sprintf("…and %d more", len(notes)-3))
		}
		report += "\n" + strings.Join(notes, "\n")
	}
	return report
}

// --- shared helpers --------------------------------------------------------

// placeholderRoom is the room as it stands: the placeholder joins (any
// member may read a room) and returns the snapshot.
func (b *Bot) placeholderRoom(ctx context.Context, roomID string) (*client.Room, error) {
	var snapshot *client.Room
	err := b.accounts.withPlaceholder(ctx, func(c *client.Client) error {
		room, err := c.JoinRoom(ctx, roomID)
		if err != nil {
			return err
		}
		snapshot, err = room.Snapshot(ctx)
		return err
	})
	return snapshot, err
}

// ownedSet is the ids a Discord user owns on the placeholder, or nil when
// they are linked and everything on the account is theirs.
func (b *Bot) ownedSet(ctx context.Context, discordID, kind string) (map[string]bool, error) {
	linked, err := b.accounts.withUser(ctx, discordID, func(*client.Client) error { return nil })
	if err != nil {
		return nil, err
	}
	if linked {
		return nil, nil
	}
	refs, err := b.store.Owned(ctx, discordID, kind)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, ref := range refs {
		set[ref] = true
	}
	return set, nil
}

// attributeFavorite records an unlinked user's favorite for the merge.
func (b *Bot) attributeFavorite(ctx context.Context, discordID, trackID string) error {
	linked, err := b.accounts.withUser(ctx, discordID, func(*client.Client) error { return nil })
	if err != nil || linked {
		return err
	}
	return b.store.Own(ctx, discordID, "favorite", trackID)
}

// findPlaylist resolves a playlist by name for the requester; an unlinked
// user only ever sees their own.
func (b *Bot) findPlaylist(ctx context.Context, c *client.Client, discordID, name string) (*client.Playlist, error) {
	owned, err := b.ownedSet(ctx, discordID, "playlist")
	if err != nil {
		return nil, err
	}
	if owned == nil {
		return c.PlaylistByName(ctx, name)
	}
	playlists, err := c.Playlists(ctx)
	if err != nil {
		return nil, err
	}
	for _, playlist := range playlists {
		if owned[playlist.ID] && strings.EqualFold(playlist.Name, name) {
			return &playlist, nil
		}
	}
	return nil, fmt.Errorf("no playlist named %q of yours", name)
}

// modalFields reads the link form's two boxes.
func modalFields(i *discordgo.InteractionCreate) (username, password string) {
	data := i.ModalSubmitData()
	for _, component := range data.Components {
		var row discordgo.ActionsRow
		switch v := component.(type) {
		case discordgo.ActionsRow:
			row = v
		case *discordgo.ActionsRow:
			row = *v
		default:
			continue
		}
		for _, child := range row.Components {
			var input discordgo.TextInput
			switch v := child.(type) {
			case discordgo.TextInput:
				input = v
			case *discordgo.TextInput:
				input = *v
			default:
				continue
			}
			switch input.CustomID {
			case "username":
				username = strings.TrimSpace(input.Value)
			case "password":
				password = input.Value
			}
		}
	}
	return username, password
}

// memberName is who a queue row was added by, falling back to the id when the
// room somehow does not name them.
func memberName(room *client.Room, memberID string) string {
	for _, member := range room.Members {
		if member.ID == memberID {
			if member.Name != "" {
				return member.Name
			}
			break
		}
	}
	return "someone"
}

// parseTimecode reads "90", "1:30" or "1:02:03" as milliseconds.
func parseTimecode(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("give a position like 1:30")
	}
	parts := strings.Split(raw, ":")
	if len(parts) > 3 {
		return 0, fmt.Errorf("%q is not a time", raw)
	}
	var seconds int64
	for _, part := range parts {
		n, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("%q is not a time", raw)
		}
		seconds = seconds*60 + n
	}
	return seconds * 1000, nil
}

// formatDuration renders milliseconds as m:ss or h:mm:ss.
func formatDuration(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	seconds := ms / 1000
	if seconds >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", seconds/3600, (seconds/60)%60, seconds%60)
	}
	return fmt.Sprintf("%d:%02d", seconds/60, seconds%60)
}

// optionRaw reads an option's raw string value — channel options are not
// strings to StringValue, which would panic.
func optionRaw(data discordgo.ApplicationCommandInteractionData, name string) string {
	for _, opt := range data.Options {
		if opt.Name == name {
			if value, ok := opt.Value.(string); ok {
				return value
			}
			return ""
		}
	}
	return ""
}

func optionInt(data discordgo.ApplicationCommandInteractionData, name string) int64 {
	for _, opt := range data.Options {
		if opt.Name == name {
			return opt.IntValue()
		}
	}
	return 0
}

// linkFor is the stored link a successful login creates.
func linkFor(discordID, username, password, token string) state.Link {
	return state.Link{
		DiscordID: discordID,
		Username:  username,
		Password:  password,
		Token:     token,
		LinkedAt:  time.Now(),
	}
}
