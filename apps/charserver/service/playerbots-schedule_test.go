package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/walkline/ToCloud9/apps/charserver/repo"
)

func scheduleMessage(t *testing.T, kind, request string, at time.Time, payload any) *nats.Msg {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(botSocialEvent{Version: 1, Type: kind, Timestamp: at.UnixMilli(), BotGUID: "population", Owner: "world", RequestID: request, Payload: data})
	if err != nil {
		t.Fatal(err)
	}
	return &nats.Msg{Subject: "test.population.events.0.world.population", Data: data}
}

func TestSocialScheduleRediscoversOfflineBotsWithoutModelOrSession(t *testing.T) {
	b := NewPlayerbotsListener(nil, affinityTestCharacters{guids: []uint64{10}}, nil, "test", 1, "v1")
	b.SetAffinityDirectory(affinityTestDirectory{characters: []repo.Character{{CharGUID: 10, CharName: "Human", CharLevel: 12, GatewayID: "human"}}})
	operations := []string{}
	var preference map[string]any
	b.schedulePublish = func(owner, request, operation string, arguments any) error {
		operations = append(operations, operation)
		if operation == "social_schedule_preferences" {
			preference = arguments.(map[string]any)
		}
		return nil
	}
	ctx := context.Background()
	if err := b.handle(ctx, scheduleMessage(t, "population_owner_online", "", time.Now(), map[string]any{})); err != nil {
		t.Fatal(err)
	}
	request := b.scheduleOwners["world"].Request
	manifest := scheduleMessage(t, "social_population_manifest", request, time.Now(), map[string]any{"realm_id": 1, "bot_guids": []uint64{99}})
	if err := b.handle(ctx, manifest); err != nil {
		t.Fatal(err)
	}
	if len(b.bots) != 0 || len(operations) != 2 || operations[1] != "social_schedule_preferences" || preference["preferred_guids"].([]uint64)[0] != 99 {
		t.Fatal("offline bot did not get authoritative scheduling preference")
	}
	if err := b.handle(ctx, manifest); err != nil || len(operations) != 2 {
		t.Fatal("replayed manifest renewed presence")
	}
}

func TestSocialScheduleRejectsStaleUnrequestedAndWrongRealmEvidence(t *testing.T) {
	b := NewPlayerbotsListener(nil, affinityTestCharacters{}, nil, "test", 1, "v1")
	b.SetAffinityDirectory(affinityTestDirectory{})
	calls := 0
	b.schedulePublish = func(string, string, string, any) error { calls++; return nil }
	ctx := context.Background()
	b.handleScheduleEvent(ctx, scheduleMessage(t, "population_owner_online", "", time.Now().Add(-4*time.Minute), map[string]any{}))
	b.handleScheduleEvent(ctx, scheduleMessage(t, "social_population_manifest", "unknown", time.Now(), map[string]any{"realm_id": 1, "bot_guids": []uint64{99}}))
	if calls != 0 {
		t.Fatal("unrequested/stale discovery accepted")
	}
	b.handleScheduleEvent(ctx, scheduleMessage(t, "population_owner_online", "", time.Now(), map[string]any{}))
	b.handleScheduleEvent(ctx, scheduleMessage(t, "social_population_manifest", b.scheduleOwners["world"].Request, time.Now(), map[string]any{"realm_id": 2, "bot_guids": []uint64{99}}))
	if calls != 1 {
		t.Fatal("wrong-realm manifest applied")
	}
}
