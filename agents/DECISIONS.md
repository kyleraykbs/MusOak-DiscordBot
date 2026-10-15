# DECISIONS

## 2026-10-14 Audio consistency stays in Go; no Python rewrite
- Decision: the audio-inconsistency work targets the existing Go pipeline
  (`internal/voice`, `internal/bot/voice.go`); a Python rewrite is rejected.
- Why: every observed defect is timing/lifecycle (a frame provider that dies
  silently, a watchdog that cannot escalate, buffer drains on seek, DAVE
  epoch holds, room-event resync churn). ffmpeg and the 20ms wire pacing are
  the same in any language, and the buffer already decouples production from
  the wire, so the language is not the variable. A rewrite would also drop
  dave-go (pure Go, no cgo) and redo the mycelion NixOS module, clan secrets
  and tag.
- Reversible: yes

## 2026-10-15 A stalled voice connection is rebuilt, not re-installed into
- Decision: `ConnSender` takes a `reconnect` hook and escalates to a rebuilt
  connection (Close + RemoveConn + open = fresh gateway, UDP socket, DAVE
  session, and a re-join) after `giveUpAfter` (5) stalled ticks, at most
  `rebuildLimit` (2) times, one bounded by `reconnectTimeout` (15s), then it
  gives up on the audio and fails `Send`.
- Why: disgo's voice conn closes and unregisters itself when its voice gateway
  drops, so re-installing a frame provider can never fix it — which is what the
  watchdog did every 4s forever, because one frame per install counted as
  progress. A stall is detected in ~10s and healed in ~1s in the normal case.
- Reversible: yes

## 2026-10-15 A seek to the current position is a no-op
- Decision: `Sink.Seek` returns without doing anything when the target is
  within one 20ms frame of `PositionMs`.
- Why: a seek costs an ffmpeg kill, a relaunch, a drain and a refill; for a
  position the listener is already at, that is a hiccup for nothing. The
  follower's drift correction never lands there (it only seeks past the
  tolerance), so this is for the room's controls.
- Reversible: yes

## 2026-10-15 A join is bounded, and its audio leg goes with the run
- Decision: `startGuild` wraps the whole join (room lookup/creation, Discord's
  voice handshake, the placeholder's room membership) in `voiceJoinTimeout`
  (20s, a var so a test can shorten it), and `participantRun` carries a
  `stopAudio` that `stop()` calls once the run's loop has left. `Bot.Close`
  closes the voice manager as well, under `shutdownGrace`.
- Why: three separate ways the bot could not come back from a stuck network
  leg. (1) `voiceHub.open` and `joinRoomAsPlaceholder` were handed the service
  lifetime, so `/join` and `/play` waited forever on a voice gateway or a
  musoak server that never answered — and the half-built connection stayed in
  the manager, so the retry joined the same dead one. (2) Every join and leave
  built a fresh `ConnSender` whose watchdog goroutine ticked a timer every two
  seconds for the life of the process: nothing ever closed it, because closing
  it is not `Sink.Stop`'s job (the sink must survive a track change). (3)
  Shutdown stopped the participants and closed the Discord gateway but left the
  voice connections up, so the bot sat in the channel until Discord timed it
  out and its voice gateway and UDP socket were never hung up.
- Reversible: yes

## 2026-10-15 Attribute a queue call from the room it answers with
- Decision: `queueTracks` records attribution from the `QueueMany` response —
  the items it added are the tail of the placeholder member's own queue, one
  batch at a time (`client.MaxQueueBatch`) — instead of snapshotting the room
  before and after the call.
- Why: the room a queue call answers with is the state after that call, so the
  pre-call snapshot was a whole-room fetch for information already in hand, and
  the post-call one a second. Two fewer round trips per song an unlinked person
  queues, and the attribution is exact under concurrent queueing: items someone
  else added arrive before ours in the response, never between our own items.
- Reversible: yes

## 2026-10-15 The wait for a first play stays, and why
- Decision: the bot keeps playing from the SDK's cache; streaming a song the
  server has not downloaded is left to the SDK (`agents/NOTES.md` has the
  evidence and what closing it would take).
- Why: a bot-side stream leg means a second room follower in this repo, and the
  pinned SDK (`a23fe10`) has no offset-carrying `MediaURL` to stream mid-song
  with, so the fix cannot be written correctly here yet. It is a dependency and
  an SDK decision, not a defect in this repo's playback path.
- Reversible: yes