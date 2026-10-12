// Package state is the bot's own memory: which Discord user is linked to
// which musoak account, what each unlinked user owns on the placeholder
// account, whose queue items are whose, and which voice channel maps to which
// room. None of this belongs on the musoak server; it is the bot's bookkeeping
// for the placeholder identity.
package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

// Link is a Discord user's musoak account.
type Link struct {
	DiscordID string
	Username  string
	Password  string
	Token     string
	LinkedAt  time.Time
}

// Owned is one thing an unlinked user created on the placeholder account.
// Kind is "playlist" (ref is the playlist id) or "favorite" (ref is the
// track id).
type Owned struct {
	DiscordID string
	Kind      string
	Ref       string
}

// Play is one attributed listen of an unlinked user.
type Play struct {
	DiscordID string
	TrackID   string
	PlayedMs  int64
	AtMs      int64
}

// Queued is a room queue item attributed to the Discord user who asked for it
// while unlinked.
type Queued struct {
	RoomID    string
	ItemID    string
	DiscordID string
	TrackID   string
}

// Store is the sqlite-backed state.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the state database at path with the file
// mode that keeps linked credentials to the owner. The umask covers sqlite's
// -wal and -shm sidecars too: the write-ahead log holds the most recent
// writes, credentials among them, and it is created by sqlite itself.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("state directory: %w", err)
		}
	}
	defer syscall.Umask(syscall.Umask(0o077)) // restore on return
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open state: %w", err)
	}
	// One connection: the bot's traffic is light and a single writer avoids
	// SQLITE_BUSY entirely.
	db.SetMaxOpenConns(1)
	schema := `
CREATE TABLE IF NOT EXISTS links (
	discord_id TEXT PRIMARY KEY,
	username   TEXT NOT NULL,
	password   TEXT NOT NULL,
	token      TEXT NOT NULL,
	linked_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS owned (
	discord_id TEXT NOT NULL,
	kind       TEXT NOT NULL,
	ref        TEXT NOT NULL,
	PRIMARY KEY (discord_id, kind, ref)
);
CREATE TABLE IF NOT EXISTS plays (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	discord_id TEXT NOT NULL,
	track_id   TEXT NOT NULL,
	played_ms  INTEGER NOT NULL,
	at_ms      INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS queued (
	room_id    TEXT NOT NULL,
	item_id    TEXT NOT NULL,
	discord_id TEXT NOT NULL,
	track_id   TEXT NOT NULL,
	PRIMARY KEY (room_id, item_id)
);
CREATE TABLE IF NOT EXISTS rooms (
	guild_id   TEXT NOT NULL,
	channel_id TEXT NOT NULL,
	room_id    TEXT NOT NULL,
	PRIMARY KEY (guild_id, channel_id)
);
`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create state schema: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		db.Close()
		return nil, fmt.Errorf("state file mode: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// --- links -----------------------------------------------------------------

// LinkOf reports the link for a Discord user, if any.
func (s *Store) LinkOf(ctx context.Context, discordID string) (Link, bool, error) {
	var l Link
	var at int64
	err := s.db.QueryRowContext(ctx,
		`SELECT discord_id, username, password, token, linked_at FROM links WHERE discord_id = ?`,
		discordID).Scan(&l.DiscordID, &l.Username, &l.Password, &l.Token, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return Link{}, false, nil
	}
	if err != nil {
		return Link{}, false, err
	}
	l.LinkedAt = time.UnixMilli(at)
	return l, true, nil
}

// PutLink records (or replaces) a Discord user's link.
func (s *Store) PutLink(ctx context.Context, l Link) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO links (discord_id, username, password, token, linked_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(discord_id) DO UPDATE SET
	username = excluded.username, password = excluded.password,
	token = excluded.token, linked_at = excluded.linked_at`,
		l.DiscordID, l.Username, l.Password, l.Token, l.LinkedAt.UnixMilli())
	return err
}

// SetToken refreshes the stored session token after a re-login.
func (s *Store) SetToken(ctx context.Context, discordID, token string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE links SET token = ? WHERE discord_id = ?`, token, discordID)
	return err
}

// Unlink drops a Discord user's link. Their musoak account and everything on
// it are untouched: there is no reverse merge.
func (s *Store) Unlink(ctx context.Context, discordID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM links WHERE discord_id = ?`, discordID)
	return err
}

// --- ownership -------------------------------------------------------------

// Own records that a Discord user created something on the placeholder
// account.
func (s *Store) Own(ctx context.Context, discordID, kind, ref string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO owned (discord_id, kind, ref) VALUES (?, ?, ?)`,
		discordID, kind, ref)
	return err
}

// Owned lists what a Discord user owns of one kind.
func (s *Store) Owned(ctx context.Context, discordID, kind string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT ref FROM owned WHERE discord_id = ? AND kind = ? ORDER BY ref`,
		discordID, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

// OwnedList returns the full ownership rows of one kind for every user, used
// when the placeholder's objects have to be mapped back to people.
func (s *Store) OwnedList(ctx context.Context, kind string) ([]Owned, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT discord_id, kind, ref FROM owned WHERE kind = ? ORDER BY discord_id, ref`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Owned
	for rows.Next() {
		var o Owned
		if err := rows.Scan(&o.DiscordID, &o.Kind, &o.Ref); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ForgetOwned consumes ownership rows, called once they have been merged.
func (s *Store) ForgetOwned(ctx context.Context, discordID, kind string, refs ...string) error {
	for _, ref := range refs {
		if _, err := s.db.ExecContext(ctx,
			`DELETE FROM owned WHERE discord_id = ? AND kind = ? AND ref = ?`,
			discordID, kind, ref); err != nil {
			return err
		}
	}
	return nil
}

// --- plays -----------------------------------------------------------------

// RecordPlay attributes one listen to a Discord user.
func (s *Store) RecordPlay(ctx context.Context, discordID, trackID string, playedMs, atMs int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO plays (discord_id, track_id, played_ms, at_ms) VALUES (?, ?, ?, ?)`,
		discordID, trackID, playedMs, atMs)
	return err
}

// Plays lists a Discord user's attributed listens in order.
func (s *Store) Plays(ctx context.Context, discordID string) ([]Play, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, discord_id, track_id, played_ms, at_ms FROM plays
WHERE discord_id = ? ORDER BY at_ms, id`, discordID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Play
	for rows.Next() {
		var p Play
		var id int64
		if err := rows.Scan(&id, &p.DiscordID, &p.TrackID, &p.PlayedMs, &p.AtMs); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ForgetPlays consumes a user's play attributions after a merge.
func (s *Store) ForgetPlays(ctx context.Context, discordID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM plays WHERE discord_id = ?`, discordID)
	return err
}

// --- queue attribution -----------------------------------------------------

// RememberQueued attributes a room queue item to the Discord user who asked
// for it. The primary key means the last writer wins, which only matters if
// ids collided (they do not).
func (s *Store) RememberQueued(ctx context.Context, q Queued) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO queued (room_id, item_id, discord_id, track_id) VALUES (?, ?, ?, ?)`,
		q.RoomID, q.ItemID, q.DiscordID, q.TrackID)
	return err
}

// QueuedBy lists a Discord user's attributed items, per room, in the order
// they were recorded.
func (s *Store) QueuedBy(ctx context.Context, discordID string) ([]Queued, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT room_id, item_id, discord_id, track_id FROM queued
WHERE discord_id = ? ORDER BY rowid`, discordID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanQueued(rows)
}

// QueuedInRoom maps a room's placeholder queue items to their owners, so a
// song starting can be attributed to whoever queued it.
func (s *Store) QueuedInRoom(ctx context.Context, roomID string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT item_id, discord_id FROM queued WHERE room_id = ?`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var itemID, discordID string
		if err := rows.Scan(&itemID, &discordID); err != nil {
			return nil, err
		}
		out[itemID] = discordID
	}
	return out, rows.Err()
}

// ForgetQueued deletes attribution rows, called when items leave a room or
// have been moved onto their owner's own queue.
func (s *Store) ForgetQueued(ctx context.Context, roomID string, itemIDs ...string) error {
	for _, itemID := range itemIDs {
		if _, err := s.db.ExecContext(ctx,
			`DELETE FROM queued WHERE room_id = ? AND item_id = ?`, roomID, itemID); err != nil {
			return err
		}
	}
	return nil
}

// PruneQueued drops attribution for items no longer in the room. live is the
// item ids the room currently holds.
func (s *Store) PruneQueued(ctx context.Context, roomID string, live map[string]bool) error {
	rows, err := s.db.QueryContext(ctx, `SELECT item_id FROM queued WHERE room_id = ?`, roomID)
	if err != nil {
		return err
	}
	var stale []string
	for rows.Next() {
		var itemID string
		if err := rows.Scan(&itemID); err != nil {
			rows.Close()
			return err
		}
		if !live[itemID] {
			stale = append(stale, itemID)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	return s.ForgetQueued(ctx, roomID, stale...)
}

func scanQueued(rows *sql.Rows) ([]Queued, error) {
	var out []Queued
	for rows.Next() {
		var q Queued
		if err := rows.Scan(&q.RoomID, &q.ItemID, &q.DiscordID, &q.TrackID); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// --- room mapping ----------------------------------------------------------

// RoomFor reports the musoak room a guild's voice channel maps to.
func (s *Store) RoomFor(ctx context.Context, guildID, channelID string) (string, bool, error) {
	var roomID string
	err := s.db.QueryRowContext(ctx,
		`SELECT room_id FROM rooms WHERE guild_id = ? AND channel_id = ?`,
		guildID, channelID).Scan(&roomID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return roomID, true, nil
}

// SetRoom records the mapping.
func (s *Store) SetRoom(ctx context.Context, guildID, channelID, roomID string) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO rooms (guild_id, channel_id, room_id) VALUES (?, ?, ?)
ON CONFLICT(guild_id, channel_id) DO UPDATE SET room_id = excluded.room_id`,
		guildID, channelID, roomID)
	return err
}

// ForgetRoom drops the mapping, called when the server no longer has the room.
func (s *Store) ForgetRoom(ctx context.Context, guildID, channelID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM rooms WHERE guild_id = ? AND channel_id = ?`, guildID, channelID)
	return err
}
