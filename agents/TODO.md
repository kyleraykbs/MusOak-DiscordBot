# TODO

## Audio consistency (from amanita's journal, 2026-10-14) - done 2026-10-15

Committed as `voice: a seek counts its own frames, and a stalled connection is
rebuilt` (ace8aab; the 0a206aa it was first written under is an orphan from
before the rebase). Evidence and message meanings: agents/NOTES.md.

1. DONE — the stalled sender. The watchdog counted any single frame of progress
   as health, so `giveUpAfter` was unreachable and it re-installed a provider
   into a dead connection every 4s forever. It now watches the *rate* over a
   real backlog, and escalates to a rebuilt connection (`voiceHub.reconnect`:
   Close + RemoveConn + open, so a fresh gateway/socket/DAVE session and a
   re-join), twice, then gives up on the audio.
2. DONE (bug found by reading the sink) — a seek never rebased `playedBase`, so
   `PositionMs` after a seek read the whole run-time ahead of the audio; the
   follower's drift correction then refused to correct until the track changed.
   Rebased at the seek's drain; a seek to the current position is a no-op.
   The small 60-280ms gaps are now labelled (`since_drain_ms`) so the next
   journal read settles whether they follow our own drains or not.
3. DONE — a 404 under the rejoin net is a gone room: logged as such, and the
   dead rows are forgotten instead of retried.
4. DONE — `AudioBufferMs`' comment said 3000; the constant says 2000.

## Follow-up in the SDK (musoak), not this repo

`participant: missed room events, resyncing` fires when an event's `Seq` is not
`lastSeq+1`, which includes a *repeat* (`last_seq == event_seq` in the journal,
once as a storm of ten in one millisecond). Each one refetches the snapshot and
reconciles, and the long dry runs (27s, 12min, 32min) all follow one within
seconds. Needs Kyle's go-ahead before touching `~/projects/musoak`.

A song the server has not downloaded yet still waits for the whole download
before a note sounds here, because a `Participant` plays local files only. The
streaming API that closes it (`MediaURL(variant, startMs)`, `MediaStreamInfo`)
is uncommitted in `~/projects/musoak` as of 2026-10-15 and the bot's pinned SDK
has only the offset-less `MediaURL`. What closing it takes, in order, and why
this repo left it alone: `agents/NOTES.md`.