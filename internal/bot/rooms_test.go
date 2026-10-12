package bot

import (
	"testing"

	"codeberg.org/kyleraykbs/musoak/pkg/client"
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
