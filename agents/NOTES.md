# NOTES

## Why the bot still waits for a whole download (2026-10-15)

The server can stream a song it has not downloaded yet: `GET
/api/v1/media/{id}?start=<ms>` answers with the provider's own fetch, marked
`X-Musoak-Stream: 1`, and `X-Musoak-Duration` carries the length. The CLI plays
from exactly that URL. The bot does not, and a first play therefore costs a
whole download — the server's provider fetch, its transcode, its probe, then the
transfer — before a note sounds. Read it, in order:

1. The bot's audio leg is `pkg/client`'s `Participant`, and a `Participant`
   hands its `Sink` a local file only: `load` looks the track up in the `Cache`
   and calls `warm` (a whole-file `cache.Fetch` → `StartDownloadAndWait`) when
   it is not there. The bot never fetches anything itself, so there is no
   stream leg for it to use and nothing in this repo to point at a URL.
2. Against the SDK the bot is pinned to (`go.mod`: `a23fe10`, 2026-10-14), the
   streaming API is not usable even if a stream leg existed:
   `Client.MediaURL(variantID string)` takes no start offset — the `?start=`
   form and `MediaStreamInfo`/`StreamInfo` are uncommitted work in
   `~/projects/musoak` (`pkg/client/client.go`, `pkg/client/media.go`) as of
   this note. A stream without the offset restarts a song the room is a minute
   into, which is worse than catching up mid-song later.

So closing this is two changes, neither in this repo's boundary: the SDK's
streaming API lands (commit it, then move the bot's pseudo-version), and then
either

- the SDK's `Participant` learns to hand the sink a URL for a variant the cache
  does not hold (one convention, the CLI's, and the drift correction stays in
  one place) — preferred; or
- the bot grows its own room follower, duplicating the follower rule, the
  prefetch, the drift correction and the local advance that `Participant`
  already implements — not preferred, and the reason it was left alone.

What is *not* the problem: the room's next songs are prefetched (three ahead,
two at a time) the moment they are queued, so a song already on disk starts
without a download. The wait is only ever for a song the bot has never held.

## Audio behaviour: read it from amanita's journal, not from the code

2026-10-14. The bot logs its own audio pipeline in detail, and the deployed
unit is the only place the real numbers exist. Everything below is one SSH
away (`ssh amanita`, key-based, no password):

    journalctl -u musoak-discord-bot -o cat --no-pager --since "-7 days"

`-o cat` matters: the default output pads the message off the screen.

What the lines mean, and which are benign:

- `voice: the audio buffer ran dry` - the connection's pull loop found the
  queue empty. `frames=3 ms=60 gaps=438` repeated = choppiness (the queue
  starving while a track plays). `ms=1928060 gaps=1` = a queue that is simply
  idle because nothing is queued; benign.
- `voice: the connection stopped pulling audio; restarting its sender` -
  the watchdog saw frames waiting and no progress, and re-installed the frame
  provider. In the Oct 14 20:28-20:32 episode this repeated every 4s with
  exactly one frame consumed per install and `failed to send audio ...
  shard is not ready` alongside: each fresh disgo audio sender dies after its
  first frame (its `handleErr` closes itself silently on a closed UDP:
  `errors.Is(err, net.ErrClosed) || errors.Is(err, ErrGatewayNotConnected)`),
  and the watchdog never gives up because one frame of progress resets its
  stall counter. `giveUpAfter` is therefore unreachable in this state.
- `failed to send audio ... shard is not ready` - disgo's
  `gatewayImpl.Send` returns `discord.ErrShardNotReady` whenever the voice
  gateway's status is not ready. It is the first-frame path of a fresh sender
  (`SetSpeaking` before the first frame), not a per-frame failure.
- `voice: holding frames while the DAVE epoch changes` /
  `frames flowing again after a DAVE epoch change` - dave-go's group change;
  frames wait in the queue, which is a short silence by design.
- `participant: drift far too large to correct; waiting for the room to move`
  - beyond `absurdDriftMs` (60s): the two positions describe different things,
  so it does not seek. `participant: the room was seeked; following it` is the
  deliberate-seek path (`roomSeekJumpMs` 2000).
- `bot: could not put the Discord queue back ... 404` - the rejoin net ran
  against a room that is gone.
- `startup failed ... invalid username or password (401)` in a tight loop is
  the clan var / placeholder-account password disagreement, and musoak's
  10/min login limit turns it into 429s; see the mycelion tag's note.

`internal/voice` and `internal/bot/guild.go:startParticipant` are where the
knobs live: `audioBufferMs` -> queue capacity (`bufferFrames`), drift
tolerance = buffer + 2000ms, `stabilityPackets` = 150 in the sink.

## The three defects behind "a lil bit inconsistent" (2026-10-15)

Found by reading the journal against the code; all three are now fixed.

1. The stall. `disgo`'s `connImpl.handleGatewayClose` calls `c.Close(ctx)`,
   which sends a voice state update with a nil channel — the bot *leaves the
   voice channel* — and unregisters the conn from the manager. Nothing tells the
   bot. The `ConnSender` keeps sending into the old conn: `SetSpeaking` returns
   `discord.ErrShardNotReady` (the voice gateway is down), which disgo logs as
   "failed to send audio" but does not treat as fatal, and then
   `udp.Write` fails on the closed socket with `net.ErrClosed`, which
   `defaultAudioSender.handleErr` turns into a *silent* `Close()`. So each fresh
   install dies after exactly one frame — which is why the journal has one
   "failed to send audio" per "restarting its sender", 200 of each, every 4s.
   The old watchdog reset its stall counter on that one frame, so it looped
   forever. Watchdog note: disgo never closes the OpusFrameProvider it is given;
   `pullProvider.Close()` is ours (it closes `live`, which makes a replaced
   install's pull return EOF).

2. The seek's position. `Sink.takeSeek` moved `posMs` but not `playedBase`, so
   `PositionMs` = target + every frame played since the track started. The SDK's
   `correctDrift` compares that with the room's position, and past
   `absurdDriftMs` (60s) it refuses to correct ("drift far too large to correct;
   waiting for the room to move") — so after any seek the bot stopped
   correcting until the track changed. Reproduced in a test: 150040 instead of
   30040.

3. The long silences follow an SDK resync, not a drain:
   `participant: missed room events, resyncing` fires on any `Seq != lastSeq+1`,
   including a repeat (`last_seq == event_seq`), and each one refetches the
   snapshot and reconciles. In the journal the 27s/12min/32min dry runs each
   start within seconds of one. That is `~/projects/musoak`, not this repo.

Reading the new labels in `voice: the audio buffer ran dry`:
`since_drain_ms` is how long ago the queue was last emptied on purpose. If it is
about the same as `ms`, our own drain started the gap (a seek, a pause); if it
is far larger, something else starved the producer.