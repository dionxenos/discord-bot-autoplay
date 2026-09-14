package player

import (
	"sync"

	"github.com/bwmarrin/discordgo"
)

// Manager owns one GuildPlayer per guild the bot is active in.
type Manager struct {
	session *discordgo.Session

	mu      sync.Mutex
	players map[string]*GuildPlayer
}

// NewManager creates a player Manager bound to a Discord session.
func NewManager(s *discordgo.Session) *Manager {
	return &Manager{
		session: s,
		players: make(map[string]*GuildPlayer),
	}
}

// Get returns the GuildPlayer for guildID, creating one if needed.
func (m *Manager) Get(guildID string) *GuildPlayer {
	m.mu.Lock()
	defer m.mu.Unlock()

	p, ok := m.players[guildID]
	if !ok {
		p = newGuildPlayer(m.session, guildID)
		m.players[guildID] = p
	}
	return p
}

// StopAll stops playback and disconnects every active guild player. Intended
// for use during graceful shutdown.
func (m *Manager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.players {
		p.Stop()
	}
}
