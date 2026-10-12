package voice

import (
	"errors"
	"time"

	"github.com/bwmarrin/discordgo"
)

// sendTimeout bounds one packet hand-off. discordgo's sender drains its
// channel every 20ms while the connection lives, so anything approaching this
// means the connection is gone and the track should not hang on it.
const sendTimeout = 2 * time.Second

// DiscordSender is Sender for a discordgo voice connection.
type DiscordSender struct {
	vc *discordgo.VoiceConnection
}

// NewDiscordSender adapts a joined voice connection.
func NewDiscordSender(vc *discordgo.VoiceConnection) *DiscordSender {
	return &DiscordSender{vc: vc}
}

// Send queues one packet for the connection's own 20ms pacing loop.
func (d *DiscordSender) Send(packet []byte) error {
	select {
	case d.vc.OpusSend <- packet:
		return nil
	case <-time.After(sendTimeout):
		return errors.New("voice: the voice connection stopped taking audio")
	}
}

// Speaking sets this member's voice state.
func (d *DiscordSender) Speaking(on bool) error {
	return d.vc.Speaking(on)
}

// Drain drops queued packets so nothing stale is sent.
func (d *DiscordSender) Drain() {
	for {
		select {
		case <-d.vc.OpusSend:
		default:
			return
		}
	}
}
