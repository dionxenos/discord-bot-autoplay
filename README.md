# Discord YouTube Music Bot

A Discord bot, written in Go, that searches YouTube for a query and plays the
first result in your voice channel.

## How it works

- [`discordgo`](https://github.com/bwmarrin/discordgo) talks to the Discord
  gateway/voice API.
- [`yt-dlp`](https://github.com/yt-dlp/yt-dlp) resolves a search query to a
  direct, playable audio stream URL (no YouTube API key needed).
- `ffmpeg`, driven by the pure-Go [`dca`](https://github.com/jonas747/dca)
  library, transcodes that stream into Opus frames which are sent straight to
  Discord. No cgo/C compiler is required.

## Prerequisites

1. **Go 1.21+**
2. **ffmpeg** on your `PATH` — https://ffmpeg.org/download.html
3. **yt-dlp** on your `PATH` — https://github.com/yt-dlp/yt-dlp#installation
   (e.g. `winget install yt-dlp.yt-dlp`, or `pip install -U yt-dlp`)
4. A Discord bot application:
   - Create one at https://discord.com/developers/applications
   - Under **Bot**, enable the **Message Content Intent** (required to read
     `!play ...` commands).
   - Copy the bot token.
   - Under **OAuth2 → URL Generator**, pick scope `bot`, and permissions
     `View Channel`, `Send Messages`, `Connect`, `Speak`. Use the generated
     URL to invite the bot to your server.

## Setup

```powershell
copy .env.example .env
# edit .env and paste your bot token
go run .
```

`.env` is optional — you can instead set `DISCORD_BOT_TOKEN` (and optionally
`BOT_PREFIX`, default `!`) as real environment variables.

## Commands

| Command                | Aliases        | Description                              |
|-------------------------|-----------------|-------------------------------------------|
| `!play <query or URL>` | `!p`            | Search YouTube, play or queue the top hit |
| `!skip`                | `!s`, `!next`   | Skip the current track                    |
| `!pause`                |                 | Pause playback                            |
| `!resume`               | `!unpause`      | Resume playback                           |
| `!queue`                | `!q`            | Show what's playing and queued            |
| `!nowplaying`           | `!np`           | Show the current track                    |
| `!stop`                 | `!leave`, `!dc` | Stop, clear the queue, and leave          |
| `!help`                 |                 | List commands                             |

You must be in a voice channel to use `!play`; the bot joins that channel.

## Project layout

```
main.go                    entrypoint, session setup, graceful shutdown
internal/bot/bot.go         command parsing and Discord message handlers
internal/youtube/youtube.go yt-dlp wrapper: search -> resolved stream URL
internal/player/            per-guild voice connection + queue + playback
```

## Notes

- Resolved stream URLs are short-lived (a few hours); the bot re-resolves on
  every `!play`, so this only matters if you hold onto a `Track` yourself.
- The bot leaves the voice channel automatically after 3 minutes of an empty
  queue.
