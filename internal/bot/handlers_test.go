package bot

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"codeberg.org/kyleraykbs/musoak/pkg/client"

	"codeberg.org/kyleraykbs/musoak-discord-bot/internal/link"
)

func TestParseTimecode(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		bad  bool
	}{
		{"90", 90_000, false},
		{"1:30", 90_000, false},
		{"0:05", 5_000, false},
		{"1:02:03", 3_723_000, false},
		{" 2:10 ", 130_000, false},
		{"", 0, true},
		{"banana", 0, true},
		{"1:2:3:4", 0, true},
		{"-5", 0, true},
	}
	for _, tc := range cases {
		got, err := parseTimecode(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("parseTimecode(%q) = %d, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseTimecode(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseTimecode(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	cases := map[int64]string{
		0:         "0:00",
		5_000:     "0:05",
		90_000:    "1:30",
		3_600_000: "1:00:00",
		3_661_000: "1:01:01",
		-1:        "0:00",
	}
	for ms, want := range cases {
		if got := formatDuration(ms); got != want {
			t.Errorf("formatDuration(%d) = %q, want %q", ms, got, want)
		}
	}
}

func TestMergeReportSpeaksPlainly(t *testing.T) {
	if got := mergeReport(link.Summary{}); got != "Nothing to move over." {
		t.Errorf("empty summary = %q", got)
	}
	got := mergeReport(link.Summary{Playlists: 2, Favorites: 3, Queued: 4, Ranking: true})
	if !strings.Contains(got, "2 playlist(s)") || !strings.Contains(got, "3 favorite(s)") ||
		!strings.Contains(got, "4 queued song(s)") || !strings.Contains(got, "provider order") {
		t.Errorf("summary report = %q", got)
	}
	// Notes are capped so one bad merge cannot fill the channel: three stay,
	// the rest are counted, and none of the dropped text is shown.
	many := link.Summary{Notes: []string{"alpha", "beta", "gamma", "delta", "epsilon"}}
	report := mergeReport(many)
	if !strings.Contains(report, "…and 2 more") {
		t.Errorf("capped notes = %q, want the count of the rest", report)
	}
	if strings.Contains(report, "delta") || strings.Contains(report, "epsilon") {
		t.Errorf("dropped notes leaked into the report: %q", report)
	}
}

func TestModalFieldsReadsBothBoxes(t *testing.T) {
	// Round-trip the way Discord delivers it: JSON in, the library's own
	// component types out — the walk has to survive whichever shape that is.
	wire := `{"type":5,"data":{"custom_id":"link-modal","components":[` +
		`{"type":1,"components":[{"type":4,"custom_id":"username","value":"  kyle "}]},` +
		`{"type":1,"components":[{"type":4,"custom_id":"password","value":"hunter2"}]}]}}`
	var create discordgo.InteractionCreate
	if err := json.Unmarshal([]byte(wire), &create); err != nil {
		t.Fatal(err)
	}
	if create.Type != discordgo.InteractionModalSubmit {
		t.Fatalf("interaction type = %v", create.Type)
	}
	username, password := modalFields(&create)
	if username != "kyle" {
		t.Errorf("username = %q, want kyle trimmed", username)
	}
	if password != "hunter2" {
		t.Errorf("password = %q", password)
	}
}

func TestMemberNameFallsBack(t *testing.T) {
	room := &client.Room{
		Members: []client.RoomMember{{ID: "m1", Name: "Kyle"}},
	}
	if got := memberName(room, "m1"); got != "Kyle" {
		t.Errorf("named member = %q", got)
	}
	if got := memberName(room, "ghost"); got != "someone" {
		t.Errorf("unknown member = %q, want the fallback", got)
	}
}
