package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

type botScheduleOwner struct {
	Seen      time.Time
	Requested time.Time
	Request   string
	Processed bool
}

// Discovery is independent of online bot sessions, so offline friends and
// charserver restarts can reconstruct preferences from the ordinary social table.
func (b *PlayerbotsListener) handleScheduleEvent(ctx context.Context, msg *nats.Msg) error {
	if b.affinityDirectory == nil {
		return nil
	}
	var event botSocialEvent
	if err := json.Unmarshal(msg.Data, &event); err != nil {
		return err
	}
	now := time.Now()
	if event.Version != 1 || event.BotGUID != "population" || event.Owner == "" || strings.ContainsAny(event.Owner, ".* >") ||
		!strings.HasSuffix(msg.Subject, "."+event.Owner+".population") || event.Timestamp <= 0 || event.Timestamp > now.UnixMilli() || now.Sub(time.UnixMilli(event.Timestamp)) > 3*time.Minute {
		return nil
	}
	if b.scheduleOwners == nil {
		b.scheduleOwners = make(map[string]*botScheduleOwner)
	}
	owner := b.scheduleOwners[event.Owner]
	if event.Type == "population_owner_online" {
		if owner == nil {
			for key, value := range b.scheduleOwners {
				if now.Sub(value.Seen) > 3*time.Minute {
					delete(b.scheduleOwners, key)
				}
			}
			if len(b.scheduleOwners) >= 32 {
				return nil
			}
			owner = &botScheduleOwner{}
			b.scheduleOwners[event.Owner] = owner
		}
		owner.Seen = now
		if now.Sub(owner.Requested) < time.Minute {
			return nil
		}
		owner.Request = "social-" + strconv.FormatInt(now.UnixNano(), 10)
		owner.Requested = now
		owner.Processed = false
		return b.publishScheduleCommand(event.Owner, owner.Request, "social_population_manifest", map[string]any{})
	}
	if event.Type != "social_population_manifest" || owner == nil || owner.Processed || event.RequestID != owner.Request || now.Sub(owner.Requested) > time.Minute {
		return nil
	}
	var manifest struct {
		Realm uint32   `json:"realm_id"`
		GUIDs []uint64 `json:"bot_guids"`
	}
	if err := json.Unmarshal(event.Payload, &manifest); err != nil {
		return err
	}
	if manifest.Realm != b.realm || len(manifest.GUIDs) > 256 {
		return nil
	}
	preferred, err := b.collectSchedulePreferences(ctx, manifest.GUIDs)
	if err != nil {
		return err
	} // Failure is unknown; let native TTL expire.
	err = b.publishScheduleCommand(event.Owner, owner.Request+"-preferences", "social_schedule_preferences", map[string]any{
		"manifest_request": owner.Request, "observed_at": time.Now().UnixMilli(), "preferred_guids": preferred,
	})
	if err == nil {
		owner.Processed = true
	}
	return err
}

func (b *PlayerbotsListener) collectSchedulePreferences(ctx context.Context, guids []uint64) ([]uint64, error) {
	if len(guids) > 256 {
		return nil, fmt.Errorf("social schedule manifest exceeds limit")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	seen := make(map[uint64]bool)
	preferred := make([]uint64, 0)
	for _, guid := range guids {
		if guid == 0 || guid > 0xffffffff || seen[guid] {
			return nil, fmt.Errorf("invalid social schedule bot GUID")
		}
		seen[guid] = true
		friends, err := b.collectAffinities(ctx, guid)
		if err != nil {
			return nil, err
		}
		for _, friend := range friends {
			if friend.Online {
				preferred = append(preferred, guid)
				break
			}
		}
	}
	return preferred, nil
}

func (b *PlayerbotsListener) publishScheduleCommand(owner, request, operation string, arguments any) error {
	if b.schedulePublish != nil {
		return b.schedulePublish(owner, request, operation, arguments)
	}
	data, err := json.Marshal(map[string]any{"request_id": request, "operation": operation, "arguments": arguments})
	if err != nil {
		return err
	}
	return b.nc.Publish(b.prefix+".population.commands."+owner, data)
}
