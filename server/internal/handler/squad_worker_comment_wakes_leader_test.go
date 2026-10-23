package handler

import "testing"

func TestGuestSquadWorkerReplayRequiresPlannedInput_GH8301(t *testing.T) {
	trigger := commentAgentTrigger{Source: commentTriggerSourceThreadParent, NonLeaderAgentReply: true}
	if got := keepReplayableAgentTriggers([]commentAgentTrigger{trigger}, false); len(got) != 0 {
		t.Fatal("timestamp-only guest worker reply must not replay")
	}
	if got := keepReplayableAgentTriggers([]commentAgentTrigger{trigger}, true); len(got) != 1 {
		t.Fatal("planned guest worker reply must replay")
	}
}
