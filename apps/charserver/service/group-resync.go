package service

import (
	"context"
	"time"

	pbGroup "github.com/walkline/ToCloud9/gen/group/pb"
	"github.com/walkline/ToCloud9/shared/events"
)

// GroupResyncHandler republishes the authoritative group state when a
// character logs in. A restarting worldserver otherwise never hears about
// groups created before it came up: the group service still counts the
// character as grouped while the core session has no group at all, which is
// the "am I in a group or not" confusion. The core's GroupCreated hook is
// idempotent, so replaying state for an already-known group is a no-op.
type GroupResyncHandler struct {
	groups    pbGroup.GroupServiceClient
	producer  events.GroupServiceProducer
	realm     uint32
	serviceID string
}

func NewGroupResyncHandler(groups pbGroup.GroupServiceClient, producer events.GroupServiceProducer,
	realm uint32, serviceID string) *GroupResyncHandler {
	return &GroupResyncHandler{groups: groups, producer: producer, realm: realm, serviceID: serviceID}
}

func (h *GroupResyncHandler) HandleCharacterLoggedIn(payload events.GWEventCharacterLoggedInPayload) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := h.groups.GetGroupByMember(ctx, &pbGroup.GetGroupByMemberRequest{
		Api: h.serviceID, RealmID: h.realm, Player: payload.CharGUID,
	})
	if err != nil {
		// No group is the normal case for most logins.
		return nil
	}
	if resp == nil || resp.Group == nil || len(resp.Group.Members) == 0 {
		return nil
	}
	members := make([]events.GroupMember, 0, len(resp.Group.Members))
	for _, member := range resp.Group.Members {
		members = append(members, events.GroupMember{
			MemberGUID: member.Guid,
			MemberName: member.Name,
			IsOnline:   member.IsOnline,
		})
	}
	return h.producer.GroupCreated(&events.GroupEventGroupCreatedPayload{
		ServiceID: h.serviceID, RealmID: h.realm,
		GroupID: uint(resp.Group.Id), LeaderGUID: resp.Group.Leader,
		LootMethod: uint8(resp.Group.LootMethod), LooterGUID: resp.Group.Looter,
		LootThreshold: uint8(resp.Group.LootThreshold), GroupType: uint8(resp.Group.GroupType),
		Difficulty: uint8(resp.Group.Difficulty), RaidDifficulty: uint8(resp.Group.RaidDifficulty),
		MasterLooterGuid: resp.Group.MasterLooter, Members: members,
	})
}
