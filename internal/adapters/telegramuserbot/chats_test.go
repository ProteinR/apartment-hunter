package telegramuserbot

import (
	"testing"

	"github.com/gotd/td/tg"
)

func TestPublicAvailableChatsReturnsPublicChannelsAndMegagroups(t *testing.T) {
	channel := &tg.Channel{ID: 10, Title: "Batumi Rent", Broadcast: true}
	channel.SetUsername("batumi_rent")
	group := &tg.Channel{ID: 20, Title: "Batumi Apartments Chat", Megagroup: true}
	group.SetUsername("batumi_apartments")
	privateGroup := &tg.Channel{ID: 30, Title: "Private Rent", Megagroup: true}
	leftChannel := &tg.Channel{ID: 40, Title: "Old Rent", Broadcast: true, Left: true}
	leftChannel.SetUsername("old_rent")

	got := publicAvailableChats([]tg.ChatClass{channel, group, privateGroup, leftChannel})

	if len(got) != 2 {
		t.Fatalf("expected two public chats, got %+v", got)
	}
	if got[0].ID != "10" || got[0].Username != "batumi_rent" || got[0].Kind != "channel" {
		t.Fatalf("unexpected channel mapping: %+v", got[0])
	}
	if got[1].ID != "20" || got[1].Username != "batumi_apartments" || got[1].Kind != "group" {
		t.Fatalf("unexpected group mapping: %+v", got[1])
	}
}
