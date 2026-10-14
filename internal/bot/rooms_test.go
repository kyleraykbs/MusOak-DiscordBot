package bot

import (
	"testing"

	"github.com/kyleraykbs/MusOak/pkg/client"

	"github.com/kyleraykbs/MusOak-DiscordBot/internal/state"
)

func roomWith(id, name string, at int64, members ...client.RoomMember) client.Room {
	return client.Room{ID: id, Name: name, CreatedAtMs: at, Members: members}
}

func TestPickRoomByNamePrefersTheBotsOwnRoom(t *testing.T) {
	member := client.RoomMember{ID: "placeholder", UserID: "placeholder"}
	other := client.RoomMember{ID: "someone"}

	rooms := []client.Room{
		roomWith("old", "Squiddy", 100, other),
		roomWith("mine", "Squiddy", 50, member, other),
		roomWith("newer", "Squiddy", 200, other),
		roomWith("elsewhere", "Kitchen", 300, member),
	}
	// The bot being in a room with the name beats newer and busier ones: that
	// is the room it made for this channel.
	if got, ok := pickRoomByName(rooms, "Squiddy", "placeholder"); !ok || got != "mine" {
		t.Errorf("picked %q (%v), want mine", got, ok)
	}

	// With the bot in none of them, somebody listening beats an empty room.
	if got, ok := pickRoomByName(rooms[:1], "Squiddy", "placeholder"); !ok || got != "old" {
		t.Errorf("picked %q (%v), want old", got, ok)
	}

	// Empty rooms of the same name: the newest is the one somebody just made.
	empties := []client.Room{
		roomWith("stale", "Squiddy", 100),
		roomWith("fresh", "Squiddy", 200),
	}
	if got, ok := pickRoomByName(empties, "Squiddy", "placeholder"); !ok || got != "fresh" {
		t.Errorf("picked %q (%v), want fresh", got, ok)
	}

	// Another channel's room is not this channel's room.
	if _, ok := pickRoomByName(rooms, "Nowhere", "placeholder"); ok {
		t.Error("picked a room with the wrong name")
	}
	if _, ok := pickRoomByName(nil, "Squiddy", "placeholder"); ok {
		t.Error("picked a room out of nothing")
	}
	if _, ok := pickRoomByName(rooms, "", "placeholder"); ok {
		t.Error("picked a room for an unnamed channel")
	}
}

func TestPlanRestorePutsBackWhatTheRoomLost(t *testing.T) {
	rows := []state.Queued{
		{RoomID: "r1", ItemID: "i1", DiscordID: "d1", TrackID: "t1"},
		{RoomID: "r1", ItemID: "i2", DiscordID: "d2", TrackID: "t2"},
		{RoomID: "r1", ItemID: "i3", DiscordID: "d1", TrackID: "t3"},
	}
	// t2 is still queued (the room kept it); t1 and t3 went with the
	// membership, so they go back — in order, under the same people.
	missing, forget := planRestore(rows, map[string]bool{"t2": true})
	if len(missing) != 2 || missing[0].TrackID != "t1" || missing[1].TrackID != "t3" {
		t.Fatalf("missing = %+v, want t1 then t3", missing)
	}
	if len(forget) != 2 || forget[0] != "i1" || forget[1] != "i3" {
		t.Fatalf("forget = %v, want the gone item ids", forget)
	}
	if missing[0].DiscordID != "d1" || missing[1].DiscordID != "d1" {
		t.Errorf("attribution lost: %+v", missing)
	}

	// Nothing lost: nothing to do, and no rows are consumed.
	missing, forget = planRestore(rows, map[string]bool{"t1": true, "t2": true, "t3": true})
	if len(missing) != 0 || len(forget) != 0 {
		t.Fatalf("missing = %+v forget = %v, want nothing", missing, forget)
	}
}

// A member who leaves a room takes their queue with them, and the placeholder
// holds every unlinked user's songs - so a leave costs those users their queue.
// What the room no longer holds is what has to be put back, in the order it was
// asked for.
func TestMissingQueuedIsWhatTheRoomLost(t *testing.T) {
	rows := []state.Queued{
		{RoomID: "r1", ItemID: "i1", DiscordID: "d1", TrackID: "t1"},
		{RoomID: "r1", ItemID: "i2", DiscordID: "d2", TrackID: "t2"},
		{RoomID: "r1", ItemID: "i3", DiscordID: "d1", TrackID: "t3"},
	}
	// Everything the room holds is left alone.
	if missing := missingQueued(rows, map[string]string{"i1": "d1", "i2": "d2", "i3": "d1"}); len(missing) != 0 {
		t.Errorf("missing = %v, want nothing", missing)
	}
	// What the room has lost is put back in the order it was queued.
	missing := missingQueued(rows, map[string]string{"i2": "d2"})
	if len(missing) != 2 || missing[0].ItemID != "i1" || missing[1].ItemID != "i3" {
		t.Fatalf("missing = %v, want i1 then i3", missing)
	}
	if missing[0].DiscordID != "d1" || missing[0].TrackID != "t1" {
		t.Errorf("the attribution went with it: %+v", missing[0])
	}
	// A room that holds none of them has lost all of them.
	if len(missingQueued(rows, nil)) != 3 {
		t.Errorf("missing = %d, want 3", len(missingQueued(rows, nil)))
	}
}
