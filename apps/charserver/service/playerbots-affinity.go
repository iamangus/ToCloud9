package service

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/walkline/ToCloud9/apps/charserver/repo"
	"github.com/walkline/ToCloud9/shared/events"
)

const botAffinityRefresh = time.Minute

type botHumanAffinity struct {
	GUID          uint64 `json:"guid"`
	Name          string `json:"name"`
	Online        bool   `json:"online"`
	PresenceKnown bool   `json:"presence_known"`
	Level         uint32 `json:"level"`
	Zone          uint32 `json:"zone_id"`
	Map           uint32 `json:"map_id"`
}

// The character social table is the durable source of consent: a human added
// this bot as a friend. Presence comes from the live gateway directory, never
// characters.online, and is refreshed even when the human is standing still.
func (b *PlayerbotsListener) SetAffinityDirectory(directory repo.CharactersOnline) {
	b.affinityDirectory = directory
}

func (b *PlayerbotsListener) refreshAffinity(ctx context.Context, bot *botPresence, now time.Time) error {
	if b.affinityDirectory == nil || b.repo == nil || now.Sub(bot.lastAffinityRefresh) < botAffinityRefresh {
		return nil
	}
	bot.lastAffinityRefresh = now
	requestCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	projection, err := b.collectAffinityProjection(requestCtx, bot.login.CharGUID)
	if err != nil {
		return err
	}
	return b.publishBotEvent(bot, "social_affinity", "", map[string]any{"realm_id": b.realm, "friends": projection.Friends, "partial": projection.Partial})
}

func (b *PlayerbotsListener) collectAffinities(requestCtx context.Context, botGUID uint64) ([]botHumanAffinity, error) {
	projection, err := b.collectAffinityProjection(requestCtx, botGUID)
	if err != nil {
		return nil, err
	}
	return projection.Friends, err
}

type botAffinityProjection struct {
	Friends []botHumanAffinity
	Partial bool
}

func (b *PlayerbotsListener) collectAffinityProjection(requestCtx context.Context, botGUID uint64) (botAffinityProjection, error) {
	projection := botAffinityProjection{Friends: make([]botHumanAffinity, 0, 8)}
	guids, err := b.repo.GetPlayersWhoHaveAsFriend(requestCtx, b.realm, botGUID)
	if err != nil {
		return projection, err
	}
	sort.Slice(guids, func(i, j int) bool { return guids[i] < guids[j] })
	// Bound database/directory work per refresh, including offline acquaintances.
	if len(guids) > 50 {
		projection.Partial = true
		guids = guids[:50]
	}
	characters, err := b.affinityDirectory.CharactersByRealmAndGUIDs(requestCtx, b.realm, guids)
	if err != nil {
		return projection, err
	}
	live := make(map[uint64]repo.Character, len(characters))
	for _, character := range characters {
		live[character.CharGUID] = character
	}
	friends := make([]botHumanAffinity, 0, len(guids))
	for _, guid := range guids {
		if guid == botGUID {
			continue
		}
		character, online := live[guid]
		if online && strings.HasPrefix(character.GatewayID, "playerbots:") {
			continue
		}
		friend := botHumanAffinity{GUID: guid, PresenceKnown: online || b.affinityOffline[guid]}
		if online {
			friend.Name, friend.Online, friend.Level, friend.Zone, friend.Map = character.CharName, true, uint32(character.CharLevel), character.CharZone, character.CharMap
		}
		friends = append(friends, friend)
	}
	sort.Slice(friends, func(i, j int) bool {
		if friends[i].Online != friends[j].Online {
			return friends[i].Online
		}
		if friends[i].PresenceKnown != friends[j].PresenceKnown {
			return !friends[i].PresenceKnown
		}
		return friends[i].GUID < friends[j].GUID
	})
	if len(friends) > 8 {
		projection.Partial = true
		friends = friends[:8]
	}
	projection.Friends = friends
	return projection, nil
}

func (b *PlayerbotsListener) observeAffinityLifecycle(data []byte, login bool) error {
	var realm uint32
	var guid uint64
	var gateway string
	if login {
		var payload events.GWEventCharacterLoggedInPayload
		if _, err := events.Unmarshal(data, &payload); err != nil {
			return err
		}
		realm, guid, gateway = payload.RealmID, payload.CharGUID, payload.GatewayID
	} else {
		var payload events.GWEventCharacterLoggedOutPayload
		if _, err := events.Unmarshal(data, &payload); err != nil {
			return err
		}
		realm, guid, gateway = payload.RealmID, payload.CharGUID, payload.GatewayID
	}
	if realm != b.realm || guid == 0 || strings.HasPrefix(gateway, "playerbots:") {
		return nil
	}
	if login {
		delete(b.affinityOffline, guid)
		return nil
	}
	if b.affinityOffline == nil {
		b.affinityOffline = make(map[uint64]bool)
	}
	if len(b.affinityOffline) >= 4096 {
		for key := range b.affinityOffline {
			delete(b.affinityOffline, key)
			break
		}
	}
	b.affinityOffline[guid] = true
	return nil
}
