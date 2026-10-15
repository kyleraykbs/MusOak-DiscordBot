package bot

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kyleraykbs/MusOak-DiscordBot/internal/musoakdtest"
	"github.com/kyleraykbs/MusOak-DiscordBot/internal/state"
)

// queueTracks is what makes an unlinked user's songs theirs: the placeholder
// member holds the items, and the bot remembers which of them this Discord user
// asked for so the merge can move them later. Working out that set used to cost
// a snapshot before the queue call and another after it - the same whole room
// fetched twice, when the room the queue call answers with already says which
// items are this member's.
func TestQueuedItemsAreAttributedWithoutASecondSnapshot(t *testing.T) {
	base := musoakdtest.StartServer(t)
	front, counter := musoakdtest.Recorder(t, base)
	ctx := context.Background()

	ph := musoakdtest.Register(t, front, testPlaceholderUser)
	one := musoakdtest.Import(t, ph, "Alpha Song.wav", 1.2)
	two := musoakdtest.Import(t, ph, "Beta Song.wav", 1.4)
	room, err := ph.CreateRoom(ctx, "Squiddy", "everyone")
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	b := testBot(t, front, store)

	join := "POST /api/v1/rooms/" + room.RoomID + "/join"
	queue := "POST /api/v1/rooms/" + room.RoomID + "/queue"
	read := "GET /api/v1/rooms/" + room.RoomID

	added, err := b.queueTracks(ctx, "discord-1", room.RoomID, []string{one, two})
	if err != nil {
		t.Fatalf("queueTracks: %v", err)
	}
	if added != 2 {
		t.Errorf("added = %d, want 2", added)
	}
	// A second person queues in the same room: what the room answered with is
	// the tail of the placeholder's queue, so the new item is theirs and the
	// first two stay attributed to the first person.
	if added, err = b.queueTracks(ctx, "discord-2", room.RoomID, []string{one}); err != nil {
		t.Fatalf("second queueTracks: %v", err)
	}
	if added != 1 {
		t.Errorf("added = %d, want 1", added)
	}

	// What those two calls cost: a join and a queue each, and no reading of
	// the whole room to work out what changed.
	if got := counter.Count(join); got != 2 {
		t.Errorf("joined the room %d times, want two (one per call)", got)
	}
	if got := counter.Count(queue); got != 2 {
		t.Errorf("queued in %d requests, want two (one per call)", got)
	}
	if got := counter.Count(read); got != 0 {
		t.Errorf("fetched the whole room %d times, want none", got)
	}

	snapshot, err := room.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	queued := snapshot.Queues[room.MemberID]
	if len(queued) != 3 {
		t.Fatalf("the placeholder holds %d items, want 3", len(queued))
	}
	owners, err := store.QueuedInRoom(ctx, room.RoomID)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"discord-1", "discord-1", "discord-2"} {
		if got := owners[queued[i].ID]; got != want {
			t.Errorf("item %d is attributed to %q, want %q", i, got, want)
		}
	}
	if queued[0].TrackID != one || queued[1].TrackID != two || queued[2].TrackID != one {
		t.Errorf("the queue is %v, want the songs in the order they were asked for",
			[]string{queued[0].TrackID, queued[1].TrackID, queued[2].TrackID})
	}
}

// A room the server no longer has - it closed while nobody was in it - is not
// an error to hand a person: the mapping is forgotten, a fresh room is opened
// for the channel, and the songs somebody queued through Discord come with it.
func TestAGoneRoomOpensAFreshOneAndKeepsTheQueue(t *testing.T) {
	base := musoakdtest.StartServer(t)
	ctx := context.Background()

	ph := musoakdtest.Register(t, base, testPlaceholderUser)
	track := musoakdtest.Import(t, ph, "Alpha Song.wav", 1.2)
	room, err := ph.CreateRoom(ctx, "Squiddy", "everyone")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := room.QueueMany(ctx, []string{track}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := room.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Queues[room.MemberID]) != 1 {
		t.Fatalf("the placeholder queued %d items, want 1", len(snapshot.Queues[room.MemberID]))
	}

	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	item := snapshot.Queues[room.MemberID][0]
	if err := store.RememberQueued(ctx, state.Queued{
		RoomID: room.RoomID, ItemID: item.ID, DiscordID: "discord-1", TrackID: track,
	}); err != nil {
		t.Fatal(err)
	}
	// The mapping as it stood while the room was there, and then the room the
	// server no longer has: the last member leaving closes it.
	if err := store.SetRoom(ctx, "g1", "c1", room.RoomID); err != nil {
		t.Fatal(err)
	}
	if err := room.Leave(ctx); err != nil {
		t.Fatal(err)
	}
	b := testBot(t, base, store)

	fresh, err := b.joinRoomAsPlaceholder(ctx, "g1", "c1", room.RoomID)
	if err != nil {
		t.Fatalf("a gone room was handed back as an error: %v", err)
	}
	if fresh.RoomID == "" || fresh.RoomID == room.RoomID {
		t.Fatalf("room = %q, want a fresh one", fresh.RoomID)
	}
	mapped, ok, err := store.RoomFor(ctx, "g1", "c1")
	if err != nil || !ok || mapped != fresh.RoomID {
		t.Errorf("the mapping is (%q, %v, %v), want the fresh room", mapped, ok, err)
	}

	// The song came along: queued in the new room, and still attributed to
	// whoever asked for it.
	after, err := fresh.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	queued := after.Queues[fresh.MemberID]
	if len(queued) != 1 || queued[0].TrackID != track {
		t.Fatalf("the fresh room holds %+v, want the queued song", queued)
	}
	owners, err := store.QueuedInRoom(ctx, fresh.RoomID)
	if err != nil {
		t.Fatal(err)
	}
	if owners[queued[0].ID] != "discord-1" {
		t.Errorf("the carried song is attributed to %q, want discord-1", owners[queued[0].ID])
	}
}
