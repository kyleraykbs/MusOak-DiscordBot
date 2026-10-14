package link

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kyleraykbs/MusOak/pkg/client"

	"github.com/kyleraykbs/MusOak-DiscordBot/internal/state"
)

// startServer runs a real musoakd against a throwaway config and returns its
// base URL. The binary comes from $MUSOAKD or the PATH — the devShell and the
// flake check both put it there — and without it there is no contract worth
// testing, so the test steps aside rather than faking the server it is here to
// pin.
func startServer(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("MUSOAKD")
	if bin == "" {
		looked, err := exec.LookPath("musoakd")
		if err != nil {
			t.Skip("musoakd not found: set MUSOAKD or run inside the devShell")
		}
		bin = looked
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	config := fmt.Sprintf("{\"listen\":%q,\"storageDir\":%q}\n", addr, filepath.Join(dir, "storage"))
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "--config", configPath)
	var log bytes.Buffer
	cmd.Stdout = &log
	cmd.Stderr = &log
	if err := cmd.Start(); err != nil {
		t.Fatalf("start musoakd: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		if t.Failed() {
			t.Logf("musoakd output:\n%s", log.String())
		}
	})

	base := "http://" + addr
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return base
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("musoakd never became healthy: %v\n%s", err, log.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// writeWAV renders a short sine as a 44.1kHz mono PCM file the server can
// import, so the test has real tracks without touching a provider.
func writeWAV(t *testing.T, name string, seconds float64) string {
	t.Helper()
	const sampleRate = 44100
	samples := int(seconds * sampleRate)
	dataBytes := samples * 2
	path := filepath.Join(t.TempDir(), name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	freq := 440.0
	if strings.HasPrefix(name, "Beta") {
		freq = 330.0
	}
	header := make([]byte, 44)
	copy(header[0:], "RIFF")
	binary.LittleEndian.PutUint32(header[4:], uint32(36+dataBytes))
	copy(header[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(header[16:], 16)
	binary.LittleEndian.PutUint16(header[20:], 1) // PCM
	binary.LittleEndian.PutUint16(header[22:], 1) // mono
	binary.LittleEndian.PutUint32(header[24:], sampleRate)
	binary.LittleEndian.PutUint32(header[28:], sampleRate*2)
	binary.LittleEndian.PutUint16(header[32:], 2)
	binary.LittleEndian.PutUint16(header[34:], 16)
	copy(header[36:], "data")
	binary.LittleEndian.PutUint32(header[40:], uint32(dataBytes))
	if _, err := f.Write(header); err != nil {
		t.Fatal(err)
	}
	samplesBuf := make([]byte, dataBytes)
	for i := range samples {
		value := 0.4 * math.Sin(2*math.Pi*freq*float64(i)/sampleRate)
		binary.LittleEndian.PutUint16(samplesBuf[i*2:], uint16(int16(value*math.MaxInt16)))
	}
	if _, err := f.Write(samplesBuf); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustRegister(t *testing.T, base, name string) *client.Client {
	t.Helper()
	auth, err := client.New(base).Register(context.Background(), name, "secret")
	if err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
	return client.New(base, client.WithToken(auth.Token))
}

func TestMergeMovesEverythingAttributable(t *testing.T) {
	base := startServer(t)
	ctx := context.Background()

	ph := mustRegister(t, base, "discord-bot")
	user := mustRegister(t, base, "kyle")
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	// Two real tracks, imported from generated audio.
	first, err := ph.ImportPath(ctx, writeWAV(t, "Alpha Song.wav", 2.1))
	if err != nil || len(first.Imported) == 0 {
		t.Fatalf("import first: %v %+v", err, first)
	}
	second, err := ph.ImportPath(ctx, writeWAV(t, "Beta Song.wav", 3.4))
	if err != nil || len(second.Imported) == 0 {
		t.Fatalf("import second: %v %+v", err, second)
	}
	one, two := first.Imported[0].Track.ID, second.Imported[0].Track.ID

	// What an unlinked Discord user builds on the placeholder account.
	playlist, err := ph.CreatePlaylist(ctx, "Road Trip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ph.AddPlaylistTracks(ctx, playlist.ID, one, two); err != nil {
		t.Fatal(err)
	}
	if err := store.Own(ctx, "discord-1", "playlist", playlist.ID); err != nil {
		t.Fatal(err)
	}
	if err := ph.AddFavorite(ctx, one); err != nil {
		t.Fatal(err)
	}
	if err := store.Own(ctx, "discord-1", "favorite", one); err != nil {
		t.Fatal(err)
	}
	if _, err := ph.SetRanking(ctx, []string{"ytmusic", "youtube"}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordPlay(ctx, "discord-1", two, 60_000, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}

	// Songs still queued in a room, attributed to them.
	room, err := ph.CreateRoom(ctx, "test room", "everyone")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := room.QueueMany(ctx, []string{one, two}); err != nil {
		t.Fatal(err)
	}
	before, err := room.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	phMember := room.MemberID
	for _, item := range before.Queues[phMember] {
		if err := store.RememberQueued(ctx, state.Queued{
			RoomID: room.RoomID, ItemID: item.ID, DiscordID: "discord-1", TrackID: item.TrackID,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if len(before.Queues[phMember]) != 2 {
		t.Fatalf("the placeholder queued %d items, want 2", len(before.Queues[phMember]))
	}

	// The link.
	merger := &Merger{Placeholder: ph, Store: store}
	sum, err := merger.Merge(ctx, "discord-1", user)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if len(sum.Notes) != 0 {
		t.Errorf("the merge reported trouble: %v", sum.Notes)
	}
	if sum.Playlists != 1 || sum.Favorites != 1 || sum.Plays != 1 || sum.Queued != 2 || !sum.Ranking {
		t.Errorf("summary = %+v, want one of each and two queue items", sum)
	}

	// Their playlists, in order, on their account; the placeholder keeps none.
	playlists, err := user.Playlists(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(playlists) != 1 || playlists[0].Name != "Road Trip" {
		t.Fatalf("your playlists = %+v", playlists)
	}
	detail, err := user.Playlist(ctx, playlists[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	var gotOrder []string
	for _, item := range detail.Items {
		gotOrder = append(gotOrder, item.Track.ID)
	}
	if len(gotOrder) != 2 || gotOrder[0] != one || gotOrder[1] != two {
		t.Errorf("playlist order = %v, want [%s %s]", gotOrder, one, two)
	}
	if theirs, err := ph.Playlists(ctx); err != nil || len(theirs) != 0 {
		t.Errorf("the placeholder kept playlists: %+v (%v)", theirs, err)
	}

	// Favorites move.
	favorites, err := user.Favorites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(favorites) != 1 || favorites[0].ID != one {
		t.Errorf("your favorites = %+v", favorites)
	}
	if theirs, err := ph.Favorites(ctx); err != nil || len(theirs) != 0 {
		t.Errorf("the placeholder kept favorites: %+v (%v)", theirs, err)
	}

	// The provider order lands on an account that had none.
	ranking, err := user.Ranking(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ranking.Ranking) != 2 || ranking.Ranking[0] != "ytmusic" || ranking.Ranking[1] != "youtube" {
		t.Errorf("your ranking = %v", ranking.Ranking)
	}

	// The listen reaches their history.
	history, err := userHistory(ctx, base, user.Token())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(history, two) {
		t.Errorf("your history does not name the played track: %s", history)
	}

	// Queued items are theirs now, in the room's own order; the placeholder's
	// queue is empty.
	after, err := room.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Queues[phMember]) != 0 {
		t.Errorf("the placeholder still holds %d queued items", len(after.Queues[phMember]))
	}
	userMember := user.MemberID()
	moved := after.Queues[userMember]
	if len(moved) != 2 || moved[0].TrackID != one || moved[1].TrackID != two {
		t.Errorf("your queue = %+v, want the two songs in order", moved)
	}

	// A second link has nothing left to move.
	again, err := merger.Merge(ctx, "discord-1", user)
	if err != nil {
		t.Fatalf("second merge: %v", err)
	}
	if again.Playlists != 0 || again.Favorites != 0 || again.Plays != 0 || again.Queued != 0 || again.Ranking {
		t.Errorf("second summary = %+v, want everything consumed", again)
	}
	if len(again.Notes) != 0 {
		t.Errorf("second merge notes: %v", again.Notes)
	}
}

// userHistory is the one history read this test needs: raw, because the SDK
// does not carry it.
func userHistory(ctx context.Context, base, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/me/history", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var body struct {
		Plays []struct {
			Track struct {
				ID string `json:"id"`
			} `json:"track"`
		} `json:"plays"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	var ids []string
	for _, play := range body.Plays {
		ids = append(ids, play.Track.ID)
	}
	return strings.Join(ids, " "), nil
}
