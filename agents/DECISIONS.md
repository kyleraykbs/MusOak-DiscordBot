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