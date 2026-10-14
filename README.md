# musoak-discord-bot

A Discord bot for [MusOak](https://github.com/kyleraykbs/MusOak): a **room per
voice channel**, songs queued by **anyone in the server** — linked people
queue as themselves, unlinked people queue under the placeholder **"Discord
Bot"** — and the room's mixed queue streamed into the voice channel.

The bot is a plain client of the musoak SDK, exactly like the CLI: it holds no
playback of its own beyond the `Sink` that puts the room's song on Discord's
voice servers.

|command|what it does|
|---|---|
|`/join [channel]`|Join a voice channel (yours by default) and open its room|
|`/leave`|Leave the voice channel; the room keeps its queue|
|`/play <query>`|Search and queue the top hit for you (auto-joins your channel)|
|`/search <query>`|Show the top hits without queueing|
|`/playlists`, `/playlist <name>`|List yours, queue one into the room|
|`/queue`, `/now`|What the room will play next, what is on now|
|`/skip`, `/pause`, `/resume`, `/seek <m:ss>`|Room controls|
|`/vote <1-5>`|Score the current song (as yourself — needs a link)|
|`/favorite`, `/favorites`|The current song to your favorites, list them|
|`/link`, `/unlink`, `/whoami`|Your MusOak account|

## How identity works

**Linked users** act as their own musoak account: their queue is theirs, the
listening board shows their name, and playlists/favorites/history are theirs
from the first command.

**Unlinked users** — anyone who has not run `/link` — queue, favorite and
playlist through a shared placeholder account whose display name is
**"Discord Bot"** (that is the name on the room's rows). The bot records who
asked for what, and `/link` **merges** that history onto their account:
playlists (copied in order, name collisions get " (from Discord)"), favorites,
their provider order when they have none, past listens, and the songs still
queued in a room move onto their own queue.

`/link` opens a form; the credentials go to the configured server only. The
bot stores the session and re-logs in with the credentials when the 30-day
session expires, so linking is one time.

## Rooms are voice channels

The first `/join` (or `/play`) in a voice channel opens a musoak room named
after it, and the mapping is remembered. Every member of the server with a
link is a member of the room; the room's mixed queue is what the bot streams.

Discord allows a bot **one voice channel per server**: `/play` from another
channel moves the bot there. Rooms are otherwise ordinary musoak rooms — open
the web UI on the same server and you will see them, queue from search, and
listen along.

## Setup

1. **A Discord application.** In the [developer portal](https://discord.com/developers/applications)
   create an application → Bot → copy the token. Invite it to your server with
   the `bot` and `applications.commands` scopes and **Connect** + **Speak** +
   **View Channels** permissions:
   `https://discord.com/oauth2/authorize?client_id=APP_ID&scope=bot%20applications.commands&permissions=3145728`
2. **A musoak account for the placeholder** — a normal account on your
   server, e.g. `musoak-discord-bot`. (If the server has registration closed,
   create it as an admin.)
3. **A config** — the bot writes you one. Run it once with no config and it
   creates a starter at `~/.config/musoak-discord-bot/config.json` (owner-only,
   since the token goes in it), prints where it is, and stops; fill it in and
   start it again. `--config PATH` overrides the path,
   `MUSOAK_DISCORD_BOT_CONFIG` sets it without the flag, and the token may
   instead come from `MUSOAK_DISCORD_TOKEN` so a deployment can keep the secret
   out of the file entirely.

   |key|meaning|
   |---|---|
   |`token`|Discord bot token; `MUSOAK_DISCORD_TOKEN` wins over the file|
   |`serverURL`|the musoak server|
   |`guilds`|servers to register commands in. Empty = every server the bot is in, registered the moment it is invited (a global set would take an hour to appear)|
   |`account.username/password`|the placeholder account|
   |`account.displayName`|defaults to `"Discord Bot"`, pinned on every start|
   |`state`|sqlite path, default `$XDG_DATA_HOME/musoak-discord-bot/state.db` (mode 0600 — it holds credentials)|
   |`cacheDir`|media cache, default `$XDG_CACHE_HOME/musoak-discord-bot`|

   Unknown keys are an error, like musoak's own configs.

4. **Run it:**

   ```sh
   nix run .#             # flake wraps ffmpeg onto PATH for you
   # or
   go run ./cmd/musoak-discord-bot --config /path/to/config.json
   ```

   A systemd user unit works like musoak's own:

   ```ini
   [Service]
   ExecStart=%h/.local/bin/musoak-discord-bot --config %h/.config/musoak-discord-bot/config.json
   Environment=MUSOAK_DISCORD_TOKEN=…   # optional: keep the token out of the file
   Restart=on-failure
   ```

## Development

```sh
nix develop              # go, gopls, ffmpeg, and a musoakd on PATH
go build ./... && go test ./...
nix flake check           # builds and runs the tests with a pinned musoakd
```

`internal/link`'s contract test spawns a **real musoakd** (`$MUSOAKD`, else
`musoakd` on PATH) and merges an unlinked history end to end; without the
binary it skips, and both the devShell and the flake check provide it.
`internal/voice`'s tests drive the sink with generated Ogg pages and the real
ffmpeg when one is present.

The bot depends on `github.com/kyleraykbs/MusOak` through a `replace` to the
GitHub mirror — the module path has no repository behind it yet; pointing the
`replace` at a future github.com/kyleraykbs/MusOak is the clean end state.

## License

GPL-2.0 — it imports musoak's SDK, which is GPL-2.0.
