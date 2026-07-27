package main

import (
	"strings"
	"sync"
	"testing"
)

func resetSlackIdentityCheckForTest() {
	slackIdentityOnce = sync.Once{}
	slackIdentityErr = nil
}

func TestSlackCallRejectsNonPersonalTokenBeforeNetwork(t *testing.T) {
	resetSlackIdentityCheckForTest()
	t.Cleanup(resetSlackIdentityCheckForTest)
	// Force the static source regardless of whatever this host's real
	// config.toml happens to carry, so this test never depends on
	// on-disk state (see slack_oauth_source_test.go).
	useStaticTokenSourceForTest(t)
	t.Setenv("SLACK_TOKEN", "xoxb-not-a-personal-token")
	if _, err := slackCall("auth.test", nil); err == nil || !strings.Contains(err.Error(), "personal xoxp-") {
		t.Fatalf("slackCall with bot token error = %v, want personal-token rejection", err)
	}
}

func TestSlackCheckExpectedIdentityEnforcesPins(t *testing.T) {
	t.Setenv("SLACK_TEAM_ID", "T_EXPECTED")
	t.Setenv("SLACK_USER_ID", "U_EXPECTED")

	if err := slackCheckExpectedIdentity(jsonObj{"team_id": "T_EXPECTED", "user_id": "U_EXPECTED"}); err != nil {
		t.Fatalf("matching identity rejected: %v", err)
	}
	if err := slackCheckExpectedIdentity(jsonObj{"team_id": "T_OTHER", "user_id": "U_EXPECTED"}); err == nil || !strings.Contains(err.Error(), "team") {
		t.Fatalf("team mismatch error = %v", err)
	}
	if err := slackCheckExpectedIdentity(jsonObj{"team_id": "T_EXPECTED", "user_id": "U_OTHER"}); err == nil || !strings.Contains(err.Error(), "user") {
		t.Fatalf("user mismatch error = %v", err)
	}
}

func TestSlackCheckExpectedIdentityAllowsLegacyUnpinnedSetup(t *testing.T) {
	t.Setenv("SLACK_TEAM_ID", "")
	t.Setenv("SLACK_USER_ID", "")
	if err := slackCheckExpectedIdentity(jsonObj{"team_id": "T_ANY", "user_id": "U_ANY"}); err != nil {
		t.Fatalf("legacy unpinned setup rejected: %v", err)
	}
}
