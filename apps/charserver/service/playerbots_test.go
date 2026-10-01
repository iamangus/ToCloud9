package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/walkline/ToCloud9/apps/charserver/repo"
	pbGroup "github.com/walkline/ToCloud9/gen/group/pb"
	pbRegistry "github.com/walkline/ToCloud9/gen/servers-registry/pb"
	"github.com/walkline/ToCloud9/shared/events"
	"google.golang.org/grpc"
)

type botTestCharacters struct{ repo.Characters }

func (botTestCharacters) CharacterByName(context.Context, uint32, string) (*repo.Character, error) {
	return &repo.Character{CharGUID: 60130, CharName: "Omaro", AccountID: 112}, nil
}

type botTestRegistry struct {
	pbRegistry.ServersRegistryServiceClient
}

func (botTestRegistry) ListGameServersForRealm(context.Context, *pbRegistry.ListGameServersForRealmRequest, ...grpc.CallOption) (*pbRegistry.ListGameServersResponse, error) {
	return &pbRegistry.ListGameServersResponse{GameServers: []*pbRegistry.GameServerDetailed{{ID: "server-id", GrpcAddress: "10.42.6.159:9509"}}}, nil
}

type botTestGroups struct {
	pbGroup.GroupServiceClient
	invite *pbGroup.InviteParams
}

func (g *botTestGroups) Invite(_ context.Context, invite *pbGroup.InviteParams, _ ...grpc.CallOption) (*pbGroup.InviteResponse, error) {
	g.invite = invite
	return &pbGroup.InviteResponse{Status: pbGroup.InviteResponse_Ok}, nil
}
func TestBotOriginatedInviteUsesOwningGameServer(t *testing.T) {
	groups := &botTestGroups{}
	b := NewPlayerbotsListener(nil, nil, groups, "playerbots.v1", 1, "0.0.1")
	b.SetRegistry(botTestRegistry{})
	bot := &botPresence{event: botSocialEvent{Owner: "10_42_6_159"}, login: events.GWEventCharacterLoggedInPayload{CharGUID: 60130, CharName: "Omaro", CharMap: 0},
		nearbyPlayers: []botNearbyPlayer{{Name: "Human", GUIDRaw: "60266", Map: 0}}}
	if status, reason := b.invite(context.Background(), bot, "human"); status != "completed" {
		t.Fatal(reason)
	}
	if groups.invite == nil || groups.invite.InviterGameServerID != "server-id" || groups.invite.Invited != 60266 {
		t.Fatal("invite lost the target or owning server binding")
	}
	bot.nearbyPlayers[0].Map = 1
	if status, _ := b.invite(context.Background(), bot, "Human"); status != "rejected" {
		t.Fatal("other-map player was invited")
	}
}

type botTestProducer struct{ logins, updates, logouts int }

func (p *botTestProducer) CharacterLoggedIn(*events.GWEventCharacterLoggedInPayload) error {
	p.logins++
	return nil
}
func (p *botTestProducer) CharactersUpdates(*events.GWEventCharactersUpdatesPayload) error {
	p.updates++
	return nil
}
func (p *botTestProducer) CharacterLoggedOut(*events.GWEventCharacterLoggedOutPayload) error {
	p.logouts++
	return nil
}

func TestBotSocialPresenceLifecycle(t *testing.T) {
	p := &botTestProducer{}
	b := NewPlayerbotsListener(nil, botTestCharacters{}, nil, "playerbots.v1", 1, "v1")
	b.producer = func(string) events.GatewayProducer { return p }
	event := botSocialEvent{Version: 1, Type: "snapshot", Timestamp: time.Now().UnixMilli(),
		BotGUID: "GUID_Full__0x000000000000eae2_Type__Player_Low__60130", Owner: "owner", Epoch: "100",
		Payload: json.RawMessage(`{"schema_version":1,"bot":{"name":"Omaro","race_id":1,"class_id":1,"level":1}}`)}
	apply := func() {
		t.Helper()
		data, _ := json.Marshal(event)
		if err := b.handle(context.Background(), &nats.Msg{Subject: "playerbots.v1.events.0.owner.bot", Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	apply()
	apply()
	if p.logins != 1 || p.updates != 1 || len(b.bots) != 1 {
		t.Fatal("snapshots must register once and update without repeated logins")
	}
	bot := b.bots[event.BotGUID]
	if bot.login.CharGUID != 60130 || bot.login.AccountID != 112 || bot.login.GatewayID != "playerbots:owner" {
		t.Fatal("bot session identity was not preserved")
	}
	event.Type, event.Epoch = "bot_offline", "older"
	apply()
	if len(b.bots) != 1 {
		t.Fatal("stale logout removed current bot")
	}
	event.Epoch = "100"
	apply()
	if p.logouts != 1 || len(b.bots) != 0 {
		t.Fatal("current logout did not remove bot")
	}
}

func TestBotSocialPresenceRejectsStaleObservations(t *testing.T) {
	b := NewPlayerbotsListener(nil, botTestCharacters{}, nil, "playerbots.v1", 1, "v1")
	event := botSocialEvent{Version: 1, Type: "snapshot", Timestamp: time.Now().Add(-3 * time.Minute).UnixMilli(),
		Owner: "owner", Epoch: "100", Payload: json.RawMessage(`{"bot":{"name":"Omaro"}}`)}
	data, _ := json.Marshal(event)
	if err := b.handle(context.Background(), &nats.Msg{Data: data}); err != nil {
		t.Fatal(err)
	}
	if len(b.bots) != 0 {
		t.Fatal("stale snapshot registered an offline bot")
	}
}

func TestBotPlayerGUIDValidation(t *testing.T) {
	for _, token := range []string{"", "population", "Creature_Low__60130", "GUID_Type__Player_Low__0", "GUID_Type__Player_Low__4294967296"} {
		if _, err := botPlayerGUID(token); err == nil {
			t.Fatalf("accepted invalid token %q", token)
		}
	}
	if guid, err := botPlayerGUID("GUID_Full__0x000000000000eae2_Type__Player_Low__60130"); err != nil || guid != 60130 {
		t.Fatal("valid player token rejected")
	}
}
