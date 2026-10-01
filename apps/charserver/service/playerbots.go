package service

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/rs/zerolog/log"
	"github.com/walkline/ToCloud9/apps/charserver/repo"
	pbGroup "github.com/walkline/ToCloud9/gen/group/pb"
	pbRegistry "github.com/walkline/ToCloud9/gen/servers-registry/pb"
	"github.com/walkline/ToCloud9/shared/events"
)

const botPresenceLease = 2 * time.Minute

type botSocialEvent struct {
	Version   int             `json:"version"`
	EventID   string          `json:"event_id"`
	Timestamp int64           `json:"timestamp_unix_ms"`
	Type      string          `json:"type"`
	BotGUID   string          `json:"bot_guid"`
	Owner     string          `json:"owner_token"`
	Epoch     string          `json:"owner_epoch"`
	RequestID string          `json:"request_id,omitempty"`
	Payload   json.RawMessage `json:"payload"`
}

type botPresence struct {
	event               botSocialEvent
	login               events.GWEventCharacterLoggedInPayload
	seen                time.Time
	pendingInvite       bool
	lastAnnouncement    time.Time
	lastAffinityRefresh time.Time
	nearbyPlayers       []botNearbyPlayer
}

type botNearbyPlayer struct {
	Name    string `json:"name"`
	GUIDRaw string `json:"guid_raw"`
	Map     uint32 `json:"map_id"`
}

// PlayerbotsListener registers socketless bot sessions in the same social
// directory as gateway sessions. Live snapshots, not saved DB online flags,
// are authoritative; leases remove sessions after a worldserver failure.
type PlayerbotsListener struct {
	nc                *nats.Conn
	repo              repo.Characters
	groups            pbGroup.GroupServiceClient
	registry          pbRegistry.ServersRegistryServiceClient
	prefix            string
	realm             uint32
	version           string
	bots              map[string]*botPresence
	producer          func(string) events.GatewayProducer
	affinityDirectory repo.CharactersOnline
	affinityOffline   map[uint64]bool
	scheduleOwners    map[string]*botScheduleOwner
	schedulePublish   func(string, string, string, any) error
}

func NewPlayerbotsListener(nc *nats.Conn, chars repo.Characters, groups pbGroup.GroupServiceClient,
	prefix string, realm uint32, version string) *PlayerbotsListener {
	return &PlayerbotsListener{nc: nc, repo: chars, groups: groups, prefix: prefix,
		realm: realm, version: version, bots: make(map[string]*botPresence),
		producer: func(owner string) events.GatewayProducer {
			return events.NewGatewayProducerNatsJSON(nc, version, realm, owner)
		}}
}

func (b *PlayerbotsListener) Run(ctx context.Context) error {
	inbox := make(chan *nats.Msg, 4096)
	var subscriptions []*nats.Subscription
	defer func() {
		for _, sub := range subscriptions {
			_ = sub.Unsubscribe()
		}
	}()
	subjects := []string{b.prefix + ".events.>", events.GroupEventInviteCreated.SubjectName(), events.GroupEventNewChatMessage.SubjectName(), b.prefix + ".social.commands.*", "chat.gw.playerbots:*.income.whisper"}
	if b.affinityDirectory != nil {
		subjects = append(subjects, b.prefix+".population.events.>", events.GWEventCharacterLoggedIn.SubjectName(), events.GWEventCharacterLoggedOut.SubjectName())
	}
	for _, subject := range subjects {
		sub, err := b.nc.Subscribe(subject, func(msg *nats.Msg) {
			select {
			case inbox <- msg:
			case <-ctx.Done():
			}
		})
		if err != nil {
			return err
		}
		subscriptions = append(subscriptions, sub)
	}
	if err := b.nc.Flush(); err != nil {
		return err
	}
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-ticker.C:
			for token, bot := range b.bots {
				if now.Sub(bot.seen) >= botPresenceLease {
					b.remove(token, bot)
				} else if err := b.refreshAffinity(ctx, bot, now); err != nil {
					log.Error().Err(err).Msg("playerbot affinity refresh failed")
				}
			}
		case msg := <-inbox:
			if err := b.handle(ctx, msg); err != nil {
				log.Error().Err(err).Str("subject", msg.Subject).Msg("playerbot social bridge")
			}
		}
	}
}

func botPlayerGUID(token string) (uint64, error) {
	const marker = "_Low__"
	index := strings.LastIndex(token, marker)
	if index < 0 || !strings.Contains(token, "_Type__Player") {
		return 0, fmt.Errorf("invalid bot player token")
	}
	guid, err := strconv.ParseUint(token[index+len(marker):], 10, 32)
	if err != nil || guid == 0 {
		return 0, fmt.Errorf("invalid bot player GUID")
	}
	return guid, nil
}

func (b *PlayerbotsListener) handle(ctx context.Context, msg *nats.Msg) error {
	if b.affinityDirectory != nil && (msg.Subject == events.GWEventCharacterLoggedIn.SubjectName() || msg.Subject == events.GWEventCharacterLoggedOut.SubjectName()) {
		return b.observeAffinityLifecycle(msg.Data, msg.Subject == events.GWEventCharacterLoggedIn.SubjectName())
	}
	if strings.HasPrefix(msg.Subject, b.prefix+".population.events.") {
		return b.handleScheduleEvent(ctx, msg)
	}
	if msg.Subject == events.GroupEventNewChatMessage.SubjectName() {
		var chat events.GroupEventNewMessagePayload
		if _, err := events.Unmarshal(msg.Data, &chat); err != nil {
			return err
		}
		if chat.RealmID != b.realm {
			return nil
		}
		for _, bot := range b.bots {
			for _, receiver := range chat.Receivers {
				if receiver == bot.login.CharGUID && receiver != chat.SenderGUID {
					kind := "party"
					if chat.MessageType == 3 {
						kind = "raid"
					}
					if err := b.publishBotEvent(bot, "chat_received", "", map[string]any{
						"chat_type": kind, "channel_type": chat.MessageType, "language": chat.Language,
						"sender_name": chat.SenderName, "sender_guid": strconv.FormatUint(chat.SenderGUID, 10), "message": chat.Msg,
					}); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	if strings.HasPrefix(msg.Subject, "chat.gw.playerbots:") {
		var whisper events.ChatEventIncomingWhisperPayload
		if _, err := events.Unmarshal(msg.Data, &whisper); err != nil {
			return err
		}
		for _, bot := range b.bots {
			if bot.login.CharGUID == whisper.ReceiverGUID && msg.Subject == events.ChatEventIncomingWhisper.SubjectName(bot.login.GatewayID) {
				return b.publishBotEvent(bot, "chat_received", "", map[string]any{
					"chat_type": "whisper", "channel_type": 7, "language": whisper.Language,
					"sender_name": whisper.SenderName, "sender_guid": strconv.FormatUint(whisper.SenderGUID, 10),
					"message": whisper.Msg,
				})
			}
		}
		return nil
	}
	if msg.Subject == events.GroupEventInviteCreated.SubjectName() {
		var invite events.GroupEventInviteCreatedPayload
		if _, err := events.Unmarshal(msg.Data, &invite); err != nil {
			return err
		}
		if invite.RealmID != b.realm {
			return nil
		}
		for _, bot := range b.bots {
			if bot.login.CharGUID == invite.InviteeGUID {
				bot.pendingInvite = true
				return b.publishBotEvent(bot, "group_invite_received", "", map[string]any{
					"inviter_name": invite.InviterName, "inviter_guid": invite.InviterGUID,
					"social_bridge": true,
				})
			}
		}
		return nil
	}
	if strings.HasPrefix(msg.Subject, b.prefix+".social.commands.") {
		return b.command(ctx, msg)
	}
	var event botSocialEvent
	if err := json.Unmarshal(msg.Data, &event); err != nil {
		return err
	}
	if event.Version != 1 || event.Owner == "" || event.Epoch == "" ||
		event.Timestamp <= 0 || time.Since(time.UnixMilli(event.Timestamp)) > botPresenceLease {
		return nil
	}
	bot := b.bots[event.BotGUID]
	if event.Type == "bot_offline" {
		if bot != nil && bot.event.Owner == event.Owner && bot.event.Epoch == event.Epoch {
			b.remove(event.BotGUID, bot)
		}
		return nil
	}
	if event.Type != "snapshot" && event.Type != "bot_online" && event.Type != "bot_heartbeat" {
		return nil
	}
	if bot != nil && event.Timestamp < bot.event.Timestamp {
		return nil
	}
	var snapshot struct {
		Bot struct {
			Name  string `json:"name"`
			Level uint8  `json:"level"`
			Class uint8  `json:"class_id"`
			Race  uint8  `json:"race_id"`
			Map   uint32 `json:"map_id"`
			Zone  uint32 `json:"zone_id"`
		} `json:"bot"`
		Snapshot      json.RawMessage   `json:"snapshot"`
		NearbyPlayers []botNearbyPlayer `json:"nearby_players"`
	}
	if err := json.Unmarshal(event.Payload, &snapshot); err != nil {
		return err
	}
	if len(snapshot.Snapshot) > 0 {
		if err := json.Unmarshal(snapshot.Snapshot, &snapshot); err != nil {
			return err
		}
	}
	if snapshot.Bot.Name == "" {
		if bot != nil && bot.event.Owner == event.Owner && bot.event.Epoch == event.Epoch {
			bot.seen = time.Now()
			return b.announce(bot)
		} else if bot == nil {
			request, _ := json.Marshal(map[string]any{
				"version": 1, "bot_guid": event.BotGUID, "owner_token": event.Owner, "owner_epoch": event.Epoch,
				"request_id": fmt.Sprintf("social-snapshot-%d", time.Now().UnixNano()), "operation": "snapshot",
				"deadline_unix_ms": time.Now().Add(30 * time.Second).UnixMilli(), "arguments": map[string]any{},
			})
			return b.nc.Publish(b.prefix+".commands."+event.Owner, request)
		}
		return nil
	}
	guid, err := botPlayerGUID(event.BotGUID)
	if err != nil {
		return err
	}
	if bot == nil {
		queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		character, err := b.repo.CharacterByName(queryCtx, b.realm, snapshot.Bot.Name)
		cancel()
		if err != nil {
			return err
		}
		if character == nil || character.CharGUID != guid {
			return fmt.Errorf("bot snapshot identity does not match character")
		}
		bot = &botPresence{login: events.GWEventCharacterLoggedInPayload{
			RealmID: b.realm, CharGUID: guid, CharName: character.CharName,
			AccountID: character.AccountID, CharGender: character.CharGender, CharGuildID: character.CharGuildID,
		}}
	}
	first := b.bots[event.BotGUID] == nil || bot.event.Owner != event.Owner || bot.event.Epoch != event.Epoch
	bot.event, bot.seen = event, time.Now()
	bot.nearbyPlayers = snapshot.NearbyPlayers
	bot.login.GatewayID = "playerbots:" + event.Owner
	bot.login.CharLevel, bot.login.CharClass, bot.login.CharRace = snapshot.Bot.Level, snapshot.Bot.Class, snapshot.Bot.Race
	bot.login.CharMap, bot.login.CharZone = snapshot.Bot.Map, snapshot.Bot.Zone
	producer := b.producer(bot.login.GatewayID)
	if first {
		if err := producer.CharacterLoggedIn(&bot.login); err != nil {
			return err
		}
		b.bots[event.BotGUID] = bot
		log.Info().Str("name", bot.login.CharName).Uint64("guid", guid).Msg("registered playerbot social session")
		return b.announce(bot)
	}
	return producer.CharactersUpdates(&events.GWEventCharactersUpdatesPayload{Updates: []*events.CharacterUpdate{{
		ID: guid, Lvl: &bot.login.CharLevel, Map: &bot.login.CharMap, Zone: &bot.login.CharZone,
	}}})
}

func (b *PlayerbotsListener) remove(token string, bot *botPresence) {
	producer := b.producer(bot.login.GatewayID)
	if err := producer.CharacterLoggedOut(&events.GWEventCharacterLoggedOutPayload{
		CharGUID: bot.login.CharGUID, CharName: bot.login.CharName,
		CharGuildID: bot.login.CharGuildID, AccountID: bot.login.AccountID,
	}); err != nil {
		log.Error().Err(err).Msg("playerbot logout publication failed")
		return
	}
	delete(b.bots, token)
}

func (b *PlayerbotsListener) publishBotEvent(bot *botPresence, kind, request string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	event := bot.event
	event.Type, event.Payload, event.RequestID = kind, data, request
	event.Timestamp = time.Now().UnixMilli()
	event.EventID = fmt.Sprintf("social-%s-%d", event.BotGUID, time.Now().UnixNano())
	encoded, err := json.Marshal(event)
	if err != nil {
		return err
	}
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(event.BotGUID))
	return b.nc.Publish(fmt.Sprintf("%s.events.%d.%s.%s", b.prefix, hash.Sum32()%256, event.Owner, event.BotGUID), encoded)
}

func (b *PlayerbotsListener) command(ctx context.Context, msg *nats.Msg) error {
	var command struct {
		BotGUID     string `json:"bot_guid"`
		Owner       string `json:"owner_token"`
		Epoch       string `json:"owner_epoch"`
		Deadline    int64  `json:"deadline_unix_ms"`
		RequestID   string `json:"request_id"`
		OperationID string `json:"operation_id"`
		Operation   string `json:"operation"`
		Arguments   struct {
			PlayerName string `json:"player_name"`
		} `json:"arguments"`
	}
	if err := json.Unmarshal(msg.Data, &command); err != nil {
		return err
	}
	bot := b.bots[command.BotGUID]
	if bot == nil || bot.event.Owner != command.Owner || bot.event.Epoch != command.Epoch ||
		command.Deadline < time.Now().UnixMilli() || command.RequestID == "" {
		return nil
	}
	status, reason := "rejected", "no pending cluster group invitation"
	if command.Operation == "invite_to_group" {
		status, reason = b.invite(ctx, bot, command.Arguments.PlayerName)
	}
	if bot.pendingInvite && command.Operation == "accept_group_invite" {
		queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		response, err := b.groups.AcceptInvite(queryCtx, &pbGroup.AcceptInviteParams{
			Api: b.version, RealmID: b.realm, Player: bot.login.CharGUID,
		})
		cancel()
		if err != nil {
			reason = "group service unavailable"
		} else if response.Status == pbGroup.AcceptInviteResponse_Ok {
			status, reason = "completed", "cluster group invitation accepted"
			bot.pendingInvite = false
		} else {
			reason = "cluster group invitation unavailable"
		}
	}
	if bot.pendingInvite && command.Operation == "decline_group_invite" {
		bot.pendingInvite = false
		status, reason = "completed", "cluster group invitation declined"
	}
	return b.publishBotEvent(bot, "operation_result", command.RequestID, map[string]any{
		"operation_id": command.OperationID, "operation": command.Operation,
		"status": status, "reason": reason, "social_bridge": true,
	})
}

func (b *PlayerbotsListener) announce(bot *botPresence) error {
	if b.registry == nil || time.Since(bot.lastAnnouncement) < time.Minute {
		return nil
	}
	if err := b.publishBotEvent(bot, "social_session_ready", "", map[string]any{}); err != nil {
		return err
	}
	bot.lastAnnouncement = time.Now()
	return nil
}

func (b *PlayerbotsListener) SetRegistry(registry pbRegistry.ServersRegistryServiceClient) {
	b.registry = registry
}

func (b *PlayerbotsListener) invite(ctx context.Context, bot *botPresence, name string) (string, string) {
	if b.registry == nil {
		return "rejected", "cluster routing unavailable"
	}
	var target uint64
	for _, player := range bot.nearbyPlayers {
		if strings.EqualFold(player.Name, name) && player.Map == bot.login.CharMap {
			target, _ = strconv.ParseUint(player.GUIDRaw, 10, 64)
			break
		}
	}
	if target == 0 || target == bot.login.CharGUID {
		return "rejected", "player not visible on current map"
	}
	queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	servers, err := b.registry.ListGameServersForRealm(queryCtx, &pbRegistry.ListGameServersForRealmRequest{Api: b.version, RealmID: b.realm})
	if err != nil {
		return "rejected", "game server registry unavailable"
	}
	serverID := ""
	for _, server := range servers.GameServers {
		host, _, err := net.SplitHostPort(server.GrpcAddress)
		if err == nil && botOwnerToken(host) == bot.event.Owner {
			if serverID != "" {
				return "rejected", "ambiguous bot game server"
			}
			serverID = server.ID
		}
	}
	if serverID == "" {
		return "rejected", "bot game server not registered"
	}
	response, err := b.groups.Invite(queryCtx, &pbGroup.InviteParams{
		Api: b.version, RealmID: b.realm, Inviter: bot.login.CharGUID, InviterName: bot.login.CharName,
		Invited: target, InvitedName: name, InviterMapID: bot.login.CharMap, InviterGameServerID: serverID,
	})
	if err != nil {
		return "rejected", "group service unavailable"
	}
	if response.Status != pbGroup.InviteResponse_Ok {
		return "rejected", "group invitation unavailable: " + response.Status.String()
	}
	return "completed", "cluster group invitation sent"
}

func botOwnerToken(host string) string {
	return strings.Map(func(character rune) rune {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' || character == '-' {
			return character
		}
		return '_'
	}, host)
}
