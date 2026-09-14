// Package bot wires up Discord message commands to the YouTube search and
// player packages.
package bot

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"musicbot/internal/player"
	"musicbot/internal/youtube"
)

const searchTimeout = 30 * time.Second

// autoQueueCount is how many related tracks are automatically queued when
// playback starts from idle, similar to Spotify's autoplay queue.
const autoQueueCount = 4

// Bot routes prefixed text commands to playback actions.
type Bot struct {
	session *discordgo.Session
	prefix  string
	manager *player.Manager
}

// New creates a Bot bound to the given session and command prefix.
func New(session *discordgo.Session, prefix string) *Bot {
	return &Bot{
		session: session,
		prefix:  prefix,
		manager: player.NewManager(session),
	}
}

// RegisterHandlers attaches the bot's Discord event handlers.
func (b *Bot) RegisterHandlers() {
	b.session.AddHandler(b.onReady)
	b.session.AddHandler(b.onMessageCreate)
}

// Shutdown stops playback and disconnects from all voice channels.
func (b *Bot) Shutdown() {
	b.manager.StopAll()
}

func (b *Bot) onReady(s *discordgo.Session, r *discordgo.Ready) {
	log.Printf("Logged in as %s", r.User.String())
	_ = s.UpdateGameStatus(0, b.prefix+"play <song>")
}

func (b *Bot) onMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author.Bot || m.GuildID == "" {
		return
	}
	if !strings.HasPrefix(m.Content, b.prefix) {
		return
	}

	content := strings.TrimSpace(strings.TrimPrefix(m.Content, b.prefix))
	if content == "" {
		return
	}

	fields := strings.SplitN(content, " ", 2)
	cmd := strings.ToLower(fields[0])
	var arg string
	if len(fields) > 1 {
		arg = strings.TrimSpace(fields[1])
	}

	switch cmd {
	case "play", "p":
		b.handlePlay(s, m, arg)
	case "skip", "s", "next":
		b.handleSkip(s, m)
	case "stop", "leave", "disconnect", "dc":
		b.handleStop(s, m)
	case "pause":
		b.handlePause(s, m)
	case "resume", "unpause":
		b.handleResume(s, m)
	case "queue", "q":
		b.handleQueue(s, m)
	case "nowplaying", "np":
		b.handleNowPlaying(s, m)
	case "help":
		b.handleHelp(s, m)
	}
}

func (b *Bot) handlePlay(s *discordgo.Session, m *discordgo.MessageCreate, query string) {
	if query == "" {
		b.reply(s, m, fmt.Sprintf("Usage: `%splay <song name or URL>`", b.prefix))
		return
	}

	voiceChannelID, err := findUserVoiceChannel(s, m.GuildID, m.Author.ID)
	if err != nil {
		b.reply(s, m, "❌ Join a voice channel first!")
		return
	}

	b.reply(s, m, fmt.Sprintf("🔎 Searching YouTube for **%s**...", query))

	ctx, cancel := context.WithTimeout(context.Background(), searchTimeout)
	defer cancel()

	result, err := youtube.SearchFirst(ctx, query)
	if err != nil {
		b.reply(s, m, "❌ Search failed: "+err.Error())
		return
	}

	track := &player.Track{
		Title:       result.Title,
		WebpageURL:  result.WebpageURL,
		Duration:    result.Duration,
		RequestedBy: m.Author.Username,
	}

	p := b.manager.Get(m.GuildID)
	position, startingNow := p.Enqueue(voiceChannelID, m.ChannelID, track)

	if !startingNow {
		b.reply(s, m, fmt.Sprintf("✅ Queued **%s** `[%s]` at position %d", track.Title, player.FormatDuration(track.Duration), position))
	} else {
		go b.autoQueueRelated(m.GuildID, voiceChannelID, m.ChannelID, track)
	}
}

// autoQueueRelated fetches tracks related to seed and appends them to the
// guild's queue, mimicking Spotify's autoplay queue. It's only invoked when
// playback starts from idle (see handlePlay), so it doesn't fire on every
// !play command.
func (b *Bot) autoQueueRelated(guildID, voiceChannelID, textChannelID string, seed *player.Track) {
	ctx, cancel := context.WithTimeout(context.Background(), searchTimeout)
	defer cancel()

	related, err := youtube.SearchRelated(ctx, seed.WebpageURL, autoQueueCount)
	if err != nil {
		log.Printf("[guild %s] autoplay: failed to fetch related tracks for %q: %v", guildID, seed.Title, err)
		return
	}

	p := b.manager.Get(guildID)
	for _, r := range related {
		p.Enqueue(voiceChannelID, textChannelID, &player.Track{
			Title:       r.Title,
			WebpageURL:  r.WebpageURL,
			Duration:    r.Duration,
			RequestedBy: "Autoplay",
		})
	}

	if len(related) > 0 {
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("➕ Autoplay added %d related track(s):\n", len(related)))
		for i, r := range related {
			fmt.Fprintf(&sb, "%d. %s `[%s]`\n", i+1, r.Title, player.FormatDuration(r.Duration))
		}
		if _, err := b.session.ChannelMessageSend(textChannelID, sb.String()); err != nil {
			log.Printf("failed to send message: %v", err)
		}
	}
}

func (b *Bot) handleSkip(s *discordgo.Session, m *discordgo.MessageCreate) {
	p := b.manager.Get(m.GuildID)
	if p.Skip() {
		b.reply(s, m, "⏭️ Skipped.")
	} else {
		b.reply(s, m, "Nothing is playing.")
	}
}

func (b *Bot) handleStop(s *discordgo.Session, m *discordgo.MessageCreate) {
	p := b.manager.Get(m.GuildID)
	p.Stop()
	b.reply(s, m, "⏹️ Stopped and cleared the queue.")
}

func (b *Bot) handlePause(s *discordgo.Session, m *discordgo.MessageCreate) {
	p := b.manager.Get(m.GuildID)
	if p.Pause() {
		b.reply(s, m, "⏸️ Paused.")
	} else {
		b.reply(s, m, "Nothing is playing.")
	}
}

func (b *Bot) handleResume(s *discordgo.Session, m *discordgo.MessageCreate) {
	p := b.manager.Get(m.GuildID)
	if p.Resume() {
		b.reply(s, m, "▶️ Resumed.")
	} else {
		b.reply(s, m, "Nothing is playing.")
	}
}

func (b *Bot) handleQueue(s *discordgo.Session, m *discordgo.MessageCreate) {
	p := b.manager.Get(m.GuildID)
	current, queue := p.Status()

	if current == nil && len(queue) == 0 {
		b.reply(s, m, "The queue is empty.")
		return
	}

	var sb strings.Builder
	if current != nil {
		fmt.Fprintf(&sb, "🎶 **Now playing:** %s `[%s]`\n", current.Title, player.FormatDuration(current.Duration))
	}
	if len(queue) > 0 {
		sb.WriteString("\n**Up next:**\n")
		max := len(queue)
		if max > 10 {
			max = 10
		}
		for i := 0; i < max; i++ {
			fmt.Fprintf(&sb, "%d. %s `[%s]`\n", i+1, queue[i].Title, player.FormatDuration(queue[i].Duration))
		}
		if len(queue) > max {
			fmt.Fprintf(&sb, "...and %d more\n", len(queue)-max)
		}
	}

	b.reply(s, m, sb.String())
}

func (b *Bot) handleNowPlaying(s *discordgo.Session, m *discordgo.MessageCreate) {
	p := b.manager.Get(m.GuildID)
	current, _ := p.Status()
	if current == nil {
		b.reply(s, m, "Nothing is playing.")
		return
	}
	b.reply(s, m, fmt.Sprintf("🎶 **Now playing:** %s `[%s]` (requested by %s)", current.Title, player.FormatDuration(current.Duration), current.RequestedBy))
}

func (b *Bot) handleHelp(s *discordgo.Session, m *discordgo.MessageCreate) {
	help := fmt.Sprintf(`**Commands** (prefix: %s)
%splay <query>  - search YouTube and play/queue the first result
%sskip          - skip the current track
%spause         - pause playback
%sresume        - resume playback
%squeue         - show the current queue
%snowplaying    - show the current track
%sstop          - stop playback, clear the queue, and leave`,
		b.prefix, b.prefix, b.prefix, b.prefix, b.prefix, b.prefix, b.prefix, b.prefix)
	b.reply(s, m, help)
}

func (b *Bot) reply(s *discordgo.Session, m *discordgo.MessageCreate, msg string) {
	if _, err := s.ChannelMessageSend(m.ChannelID, msg); err != nil {
		log.Printf("failed to send message: %v", err)
	}
}

func findUserVoiceChannel(s *discordgo.Session, guildID, userID string) (string, error) {
	guild, err := s.State.Guild(guildID)
	if err != nil {
		return "", err
	}
	for _, vs := range guild.VoiceStates {
		if vs.UserID == userID {
			return vs.ChannelID, nil
		}
	}
	return "", errors.New("user is not in a voice channel")
}
