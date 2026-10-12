package state

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestStateFileIsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("state file mode = %o, want 600", perm)
	}
}

func TestTheWriteAheadLogIsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	// A write opens the WAL, which sqlite creates under our umask.
	if err := s.PutLink(context.Background(), Link{
		DiscordID: "d1", Username: "u", Password: "p", Token: "t", LinkedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	for _, sidecar := range []string{path + "-wal", path + "-shm"} {
		info, err := os.Stat(sidecar)
		if os.IsNotExist(err) {
			continue // a checkpoint may already have removed it
		}
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s mode = %o, want 600 (it can hold credentials)", filepath.Base(sidecar), perm)
		}
	}
}

func TestLinkRoundTripAndReplace(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)

	if _, ok, err := s.LinkOf(ctx, "d1"); err != nil || ok {
		t.Fatalf("LinkOf on nobody = (%v, %v), want (zero, false)", ok, err)
	}

	at := time.UnixMilli(1_700_000_000_000)
	if err := s.PutLink(ctx, Link{DiscordID: "d1", Username: "kyle", Password: "pw", Token: "tok1", LinkedAt: at}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.LinkOf(ctx, "d1")
	if err != nil || !ok {
		t.Fatalf("LinkOf = (%v, %v)", ok, err)
	}
	if got.Username != "kyle" || got.Token != "tok1" || !got.LinkedAt.Equal(at) {
		t.Errorf("link = %+v", got)
	}

	// A second link replaces the first, not a second row.
	if err := s.PutLink(ctx, Link{DiscordID: "d1", Username: "kyle", Password: "pw2", Token: "tok2", LinkedAt: at}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetToken(ctx, "d1", "tok3"); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.LinkOf(ctx, "d1")
	if got.Token != "tok3" || got.Password != "pw2" {
		t.Errorf("after replace+settoken: %+v", got)
	}

	if err := s.Unlink(ctx, "d1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.LinkOf(ctx, "d1"); err != nil || ok {
		t.Errorf("after unlink: (%v, %v), want gone", ok, err)
	}
}

func TestOwnershipIsPerUserAndConsumable(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)

	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.Own(ctx, "d1", "playlist", "p1"))
	must(s.Own(ctx, "d1", "playlist", "p2"))
	must(s.Own(ctx, "d2", "playlist", "p3"))
	must(s.Own(ctx, "d1", "favorite", "t1"))
	// Owning the same thing twice is the same ownership.
	must(s.Own(ctx, "d1", "playlist", "p1"))

	refs, err := s.Owned(ctx, "d1", "playlist")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || refs[0] != "p1" || refs[1] != "p2" {
		t.Errorf("d1's playlists = %v, want [p1 p2]", refs)
	}
	favs, _ := s.Owned(ctx, "d1", "favorite")
	if len(favs) != 1 || favs[0] != "t1" {
		t.Errorf("d1's favorites = %v, want [t1]", favs)
	}

	must(s.ForgetOwned(ctx, "d1", "playlist", "p1"))
	refs, _ = s.Owned(ctx, "d1", "playlist")
	if len(refs) != 1 || refs[0] != "p2" {
		t.Errorf("after consuming p1: %v, want [p2]", refs)
	}

	all, err := s.OwnedList(ctx, "playlist")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("OwnedList = %+v, want d1/p2 and d2/p3", all)
	}
}

func TestPlaysRecordAndConsume(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)

	for i, p := range []struct {
		track string
		ms    int64
		at    int64
	}{{"t1", 120_000, 1000}, {"t2", 60_000, 2000}} {
		if err := s.RecordPlay(ctx, "d1", p.track, p.ms, p.at); err != nil {
			t.Fatalf("play %d: %v", i, err)
		}
	}
	if err := s.RecordPlay(ctx, "d2", "t3", 30_000, 1500); err != nil {
		t.Fatal(err)
	}

	plays, err := s.Plays(ctx, "d1")
	if err != nil {
		t.Fatal(err)
	}
	if len(plays) != 2 || plays[0].TrackID != "t1" || plays[1].TrackID != "t2" {
		t.Fatalf("d1's plays = %+v", plays)
	}
	if plays[0].PlayedMs != 120_000 || plays[0].AtMs != 1000 {
		t.Errorf("play = %+v", plays[0])
	}

	if err := s.ForgetPlays(ctx, "d1"); err != nil {
		t.Fatal(err)
	}
	if plays, _ := s.Plays(ctx, "d1"); len(plays) != 0 {
		t.Errorf("plays after merge: %v", plays)
	}
	if plays, _ := s.Plays(ctx, "d2"); len(plays) != 1 {
		t.Errorf("d2's plays disturbed by d1's merge: %v", plays)
	}
}

func TestQueuedAttributionLookupAndPrune(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)

	for _, q := range []Queued{
		{RoomID: "r1", ItemID: "i1", DiscordID: "d1", TrackID: "t1"},
		{RoomID: "r1", ItemID: "i2", DiscordID: "d2", TrackID: "t2"},
		{RoomID: "r2", ItemID: "i3", DiscordID: "d1", TrackID: "t3"},
	} {
		if err := s.RememberQueued(ctx, q); err != nil {
			t.Fatal(err)
		}
	}

	by, err := s.QueuedBy(ctx, "d1")
	if err != nil {
		t.Fatal(err)
	}
	if len(by) != 2 || by[0].RoomID != "r1" || by[1].RoomID != "r2" {
		t.Errorf("d1's queued = %+v", by)
	}

	owners, err := s.QueuedInRoom(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if owners["i1"] != "d1" || owners["i2"] != "d2" || len(owners) != 2 {
		t.Errorf("owners = %v", owners)
	}

	// i2 left the room; i1 is still there.
	if err := s.PruneQueued(ctx, "r1", map[string]bool{"i1": true}); err != nil {
		t.Fatal(err)
	}
	if owners, _ := s.QueuedInRoom(ctx, "r1"); len(owners) != 1 || owners["i1"] != "d1" {
		t.Errorf("after prune: %v", owners)
	}
	if by, _ := s.QueuedBy(ctx, "d2"); len(by) != 0 {
		t.Errorf("d2 still attributed to a gone item: %+v", by)
	}
}

func TestRoomMappingRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)

	if _, ok, err := s.RoomFor(ctx, "g1", "c1"); err != nil || ok {
		t.Fatalf("RoomFor nobody = (%v, %v)", ok, err)
	}
	if err := s.SetRoom(ctx, "g1", "c1", "room-a"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRoom(ctx, "g1", "c1", "room-b"); err != nil {
		t.Fatal(err)
	}
	roomID, ok, err := s.RoomFor(ctx, "g1", "c1")
	if err != nil || !ok || roomID != "room-b" {
		t.Errorf("RoomFor = (%q, %v, %v), want room-b", roomID, ok, err)
	}
	if err := s.ForgetRoom(ctx, "g1", "c1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.RoomFor(ctx, "g1", "c1"); ok {
		t.Error("mapping survived ForgetRoom")
	}
}
