// Package link ties a Discord user to their musoak account and merges the
// history they built up while unlinked — playlists, favorites, provider
// ranking, plays and room queue items — off the placeholder "Discord Bot"
// account onto their own.
package link

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"

	"codeberg.org/kyleraykbs/musoak/pkg/client"

	"codeberg.org/kyleraykbs/musoak-discord-bot/internal/state"
)

// playSource is the source musoak's history shows for plays the bot records.
const playSource = "discord-bot"

// Summary reports what a merge moved. Notes carry everything that did not
// move cleanly: a merge runs across two accounts and cannot be transactional,
// so it is best-effort per item and honest about what it left behind.
type Summary struct {
	Playlists int
	Favorites int
	Plays     int
	Queued    int
	Ranking   bool
	Notes     []string
}

// Merger moves one user's unlinked history from the placeholder account.
type Merger struct {
	Placeholder *client.Client
	Store       *state.Store
	Logger      *slog.Logger
}

// Merge copies everything attributable to discordID from the placeholder
// account onto the account behind user, then consumes the attributions that
// succeeded. Items that fail stay attributed, so a later link tries them again.
func (m *Merger) Merge(ctx context.Context, discordID string, user *client.Client) (Summary, error) {
	var sum Summary
	if m.Logger == nil {
		m.Logger = slog.Default()
	}

	m.mergePlaylists(ctx, discordID, user, &sum)
	m.mergeFavorites(ctx, discordID, user, &sum)
	m.mergeRanking(ctx, discordID, user, &sum)
	m.mergePlays(ctx, discordID, user, &sum)
	m.mergeQueued(ctx, discordID, user, &sum)

	if len(sum.Notes) > 0 {
		m.Logger.Warn("link: merge finished with notes",
			"discord", discordID, "notes", sum.Notes)
	}
	return sum, nil
}

// mergePlaylists recreates each playlist the user made while unlinked, in
// order, and deletes the placeholder's copy once theirs exists whole.
func (m *Merger) mergePlaylists(ctx context.Context, discordID string, user *client.Client, sum *Summary) {
	refs, err := m.Store.Owned(ctx, discordID, "playlist")
	if err != nil {
		sum.Notes = append(sum.Notes, fmt.Sprintf("reading owned playlists: %v", err))
		return
	}
	if len(refs) == 0 {
		return
	}

	existing, err := user.Playlists(ctx)
	if err != nil {
		sum.Notes = append(sum.Notes, fmt.Sprintf("listing your playlists: %v", err))
		return
	}
	taken := map[string]bool{}
	for _, p := range existing {
		taken[p.Name] = true
	}

	var copied []string
	for _, ref := range refs {
		detail, err := m.Placeholder.Playlist(ctx, ref)
		if err != nil {
			if client.IsNotFound(err) {
				// Already gone; there is nothing to copy.
				copied = append(copied, ref)
				continue
			}
			sum.Notes = append(sum.Notes, fmt.Sprintf("playlist %s: %v", ref, err))
			continue
		}

		name := freeName(detail.Name, taken)
		created, err := user.CreatePlaylist(ctx, name)
		if err != nil {
			sum.Notes = append(sum.Notes, fmt.Sprintf("creating playlist %q: %v", name, err))
			continue
		}
		taken[name] = true

		trackIDs := orderedTrackIDs(detail)
		if len(trackIDs) > 0 {
			if _, err := user.AddPlaylistTracks(ctx, created.ID, trackIDs...); err != nil {
				sum.Notes = append(sum.Notes,
					fmt.Sprintf("filling playlist %q: %v (the placeholder keeps the original)", name, err))
				// Their copy is empty; leave the original behind and retry later.
				_ = user.DeletePlaylist(ctx, created.ID)
				delete(taken, name)
				continue
			}
		}
		if err := m.Placeholder.DeletePlaylist(ctx, ref); err != nil {
			sum.Notes = append(sum.Notes,
				fmt.Sprintf("playlist %q copied but the placeholder copy could not be removed: %v", name, err))
			copied = append(copied, ref)
			continue
		}
		copied = append(copied, ref)
		sum.Playlists++
	}
	if len(copied) > 0 {
		if err := m.Store.ForgetOwned(ctx, discordID, "playlist", copied...); err != nil {
			sum.Notes = append(sum.Notes, fmt.Sprintf("remembering copied playlists: %v", err))
		}
	}
}

// mergeFavorites moves the favorited tracks over.
func (m *Merger) mergeFavorites(ctx context.Context, discordID string, user *client.Client, sum *Summary) {
	refs, err := m.Store.Owned(ctx, discordID, "favorite")
	if err != nil {
		sum.Notes = append(sum.Notes, fmt.Sprintf("reading owned favorites: %v", err))
		return
	}
	var moved []string
	for _, trackID := range refs {
		if err := user.AddFavorite(ctx, trackID); err != nil {
			sum.Notes = append(sum.Notes, fmt.Sprintf("favorite %s: %v", trackID, err))
			continue
		}
		// Best effort: the favorite stands on the user's account either way;
		// a placeholder that keeps it is invisible to them.
		_ = m.Placeholder.RemoveFavorite(ctx, trackID)
		moved = append(moved, trackID)
		sum.Favorites++
	}
	if len(moved) > 0 {
		if err := m.Store.ForgetOwned(ctx, discordID, "favorite", moved...); err != nil {
			sum.Notes = append(sum.Notes, fmt.Sprintf("remembering moved favorites: %v", err))
		}
	}
}

// mergeRanking copies the placeholder's provider order only when the user has
// none of their own: a fresh account's order is the default, and the default
// is exactly what a placeholder that nobody configured also has.
func (m *Merger) mergeRanking(ctx context.Context, discordID string, user *client.Client, sum *Summary) {
	mine, err := user.Ranking(ctx)
	if err != nil {
		sum.Notes = append(sum.Notes, fmt.Sprintf("reading your provider order: %v", err))
		return
	}
	if len(mine.Ranking) > 0 {
		return
	}
	theirs, err := m.Placeholder.Ranking(ctx)
	if err != nil || len(theirs.Ranking) == 0 {
		return
	}
	if _, err := user.SetRanking(ctx, theirs.Ranking); err != nil {
		sum.Notes = append(sum.Notes, fmt.Sprintf("copying the provider order: %v", err))
		return
	}
	sum.Ranking = true
}

// mergePlays replays the user's attributed listens onto their account. The
// server stamps each play at "now minus what they heard", so merged listens
// land in their history as recent plays of those songs; the original times are
// not something the API can carry.
func (m *Merger) mergePlays(ctx context.Context, discordID string, user *client.Client, sum *Summary) {
	plays, err := m.Store.Plays(ctx, discordID)
	if err != nil {
		sum.Notes = append(sum.Notes, fmt.Sprintf("reading attributed plays: %v", err))
		return
	}
	failed := 0
	for _, play := range plays {
		if err := recordPlay(ctx, user.BaseURL(), user.Token(), play); err != nil {
			failed++
			sum.Notes = append(sum.Notes, fmt.Sprintf("play of %s: %v", play.TrackID, err))
			continue
		}
		sum.Plays++
	}
	// Attribution is consumed either way: replays would duplicate history on
	// a retry, which is worse than dropping the few that failed.
	if err := m.Store.ForgetPlays(ctx, discordID); err != nil {
		sum.Notes = append(sum.Notes, fmt.Sprintf("forgetting merged plays: %v", err))
	}
	if failed > 0 {
		sum.Notes = append(sum.Notes, fmt.Sprintf("%d listen(s) could not be replayed", failed))
	}
}

// mergeQueued moves items still sitting in a room's placeholder queue onto
// the user's own queue, in the order the room holds them. The copy goes first
// and the placeholder's removal second: a failure then leaves a duplicate
// rather than a lost song.
func (m *Merger) mergeQueued(ctx context.Context, discordID string, user *client.Client, sum *Summary) {
	rows, err := m.Store.QueuedBy(ctx, discordID)
	if err != nil {
		sum.Notes = append(sum.Notes, fmt.Sprintf("reading attributed queue items: %v", err))
		return
	}
	byRoom := map[string][]state.Queued{}
	var roomOrder []string
	for _, row := range rows {
		if _, seen := byRoom[row.RoomID]; !seen {
			roomOrder = append(roomOrder, row.RoomID)
		}
		byRoom[row.RoomID] = append(byRoom[row.RoomID], row)
	}

	for _, roomID := range roomOrder {
		m.mergeRoomQueue(ctx, discordID, user, roomID, byRoom[roomID], sum)
	}
}

func (m *Merger) mergeRoomQueue(ctx context.Context, discordID string, user *client.Client,
	roomID string, rows []state.Queued, sum *Summary) {

	wanted := map[string]bool{}
	for _, row := range rows {
		wanted[row.ItemID] = true
	}
	forget := func(itemIDs ...string) {
		if err := m.Store.ForgetQueued(ctx, roomID, itemIDs...); err != nil {
			sum.Notes = append(sum.Notes, fmt.Sprintf("forgetting queue items: %v", err))
		}
	}

	phRoom, err := m.Placeholder.JoinRoom(ctx, roomID)
	if err != nil {
		if client.IsNotFound(err) {
			// The room closed; the queue went with it.
			var gone []string
			for _, row := range rows {
				gone = append(gone, row.ItemID)
			}
			forget(gone...)
			sum.Notes = append(sum.Notes, fmt.Sprintf("room %s is gone; its queued items were dropped", roomID))
			return
		}
		sum.Notes = append(sum.Notes, fmt.Sprintf("rejoining room %s: %v", roomID, err))
		return
	}
	snapshot, err := phRoom.Snapshot(ctx)
	if err != nil {
		sum.Notes = append(sum.Notes, fmt.Sprintf("reading room %s: %v", roomID, err))
		return
	}

	// Walk the room's own copy of the placeholder queue: that is the order
	// the songs will play in, and items already played or removed are not in
	// it any more.
	var live []client.QueueItem
	liveIDs := map[string]bool{}
	for _, item := range snapshot.Queues[phRoom.MemberID] {
		if wanted[item.ID] {
			live = append(live, item)
			liveIDs[item.ID] = true
		}
	}
	var stale []string
	for _, row := range rows {
		if !liveIDs[row.ItemID] {
			stale = append(stale, row.ItemID)
		}
	}
	if len(stale) > 0 {
		forget(stale...)
	}
	if len(live) == 0 {
		return
	}

	userRoom, err := user.JoinRoom(ctx, roomID)
	if err != nil {
		sum.Notes = append(sum.Notes, fmt.Sprintf("joining room %s: %v", roomID, err))
		return
	}
	trackIDs := make([]string, 0, len(live))
	for _, item := range live {
		trackIDs = append(trackIDs, item.TrackID)
	}
	if _, err := userRoom.QueueMany(ctx, trackIDs); err != nil {
		sum.Notes = append(sum.Notes, fmt.Sprintf("re-queueing in room %s: %v", roomID, err))
		return
	}
	for _, item := range live {
		if _, err := phRoom.Remove(ctx, item.ID); err != nil {
			sum.Notes = append(sum.Notes,
				fmt.Sprintf("item %s now plays twice (remove from the placeholder failed: %v)", item.ID, err))
		}
	}
	var moved []string
	for _, item := range live {
		moved = append(moved, item.ID)
	}
	forget(moved...)
	sum.Queued += len(live)
}

// recordPlay is the one call the SDK does not have: POST /me/plays.
func recordPlay(ctx context.Context, baseURL, token string, play state.Play) error {
	body, err := json.Marshal(map[string]any{
		"trackId":  play.TrackID,
		"playedMs": play.PlayedMs,
		"source":   playSource,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		baseURL+"/api/v1/me/plays", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("POST /me/plays: %s", resp.Status)
	}
	return nil
}

// orderedTrackIDs is a playlist's tracks in position order.
func orderedTrackIDs(detail *client.PlaylistDetail) []string {
	items := append([]client.PlaylistItem(nil), detail.Items...)
	sort.SliceStable(items, func(i, j int) bool { return items[i].Position < items[j].Position })
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.Track.ID)
	}
	return ids
}

// freeName finds a name for a copied playlist that does not collide with one
// the user already has.
func freeName(base string, taken map[string]bool) string {
	if base == "" {
		base = "Playlist"
	}
	if !taken[base] {
		return base
	}
	candidate := base + " (from Discord)"
	for i := 2; taken[candidate]; i++ {
		candidate = fmt.Sprintf("%s (from Discord %d)", base, i)
	}
	return candidate
}

// ErrNotLinked is what a command says when the Discord user has not linked.
var ErrNotLinked = errors.New("this Discord account is not linked; use /link")
