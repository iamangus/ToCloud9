package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/walkline/ToCloud9/apps/charserver/repo"
	"github.com/walkline/ToCloud9/shared/events"
)

type affinityTestCharacters struct {
	repo.Characters
	guids []uint64
}

func (r affinityTestCharacters) GetPlayersWhoHaveAsFriend(context.Context, uint32, uint64) ([]uint64, error) {
	return append([]uint64(nil), r.guids...), nil
}

type affinityTestDirectory struct {
	repo.CharactersOnline
	characters []repo.Character
	err        error
}

func (r affinityTestDirectory) CharactersByRealmAndGUIDs(context.Context, uint32, []uint64) ([]repo.Character, error) {
	return r.characters, r.err
}

func TestAffinityUsesExplicitFriendshipAndGatewayPresence(t *testing.T) {
	b := NewPlayerbotsListener(nil, affinityTestCharacters{guids: []uint64{30, 20, 10}}, nil, "test", 1, "v1")
	b.SetAffinityDirectory(affinityTestDirectory{characters: []repo.Character{
		{CharGUID: 10, CharName: "Human", CharLevel: 12, CharZone: 12, GatewayID: "gateway-1"},
		{CharGUID: 20, CharName: "Bot", CharLevel: 12, GatewayID: "playerbots:owner"},
	}})
	friends, err := b.collectAffinities(context.Background(), 99)
	if err != nil || len(friends) != 2 || friends[0].GUID != 10 || !friends[0].Online || friends[0].Level != 12 || friends[1].GUID != 30 || friends[1].Online {
		t.Fatalf("wrong consent/presence projection: %+v %v", friends, err)
	}
	b.repo = affinityTestCharacters{}
	friends, err = b.collectAffinities(context.Background(), 99)
	if err != nil || len(friends) != 0 {
		t.Fatal("removed friendship persisted")
	}
}

func TestAffinityDirectoryFailureIsNotAnOfflineObservation(t *testing.T) {
	b := NewPlayerbotsListener(nil, affinityTestCharacters{guids: []uint64{10}}, nil, "test", 1, "v1")
	b.SetAffinityDirectory(affinityTestDirectory{err: errors.New("directory unavailable")})
	if friends, err := b.collectAffinities(context.Background(), 99); err == nil || friends != nil {
		t.Fatal("directory error became logout evidence")
	}
}

func TestAffinityMissingAfterRestartIsUnknownUntilExplicitLogout(t *testing.T) {
	b := NewPlayerbotsListener(nil, affinityTestCharacters{guids: []uint64{10}}, nil, "test", 1, "v1")
	b.SetAffinityDirectory(affinityTestDirectory{})
	friends, err := b.collectAffinities(context.Background(), 99)
	if err != nil || friends[0].PresenceKnown {
		t.Fatal("empty restart directory inferred a logout")
	}
	data, err := json.Marshal(events.EventToSendGenericPayload{Payload: events.GWEventCharacterLoggedOutPayload{RealmID: 1, CharGUID: 10, GatewayID: "human"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.observeAffinityLifecycle(data, false); err != nil {
		t.Fatal(err)
	}
	friends, err = b.collectAffinities(context.Background(), 99)
	if err != nil || !friends[0].PresenceKnown || friends[0].Online {
		t.Fatal("explicit logout did not establish offline presence")
	}
	data, err = json.Marshal(events.EventToSendGenericPayload{Payload: events.GWEventCharacterLoggedInPayload{RealmID: 1, CharGUID: 10, GatewayID: "human"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.observeAffinityLifecycle(data, true); err != nil {
		t.Fatal(err)
	}
	friends, err = b.collectAffinities(context.Background(), 99)
	if err != nil || friends[0].PresenceKnown {
		t.Fatal("login did not clear the offline tombstone before directory reconciliation")
	}
}
