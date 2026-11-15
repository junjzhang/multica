package migrations

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestMigrationNumericPrefixesAreUnique(t *testing.T) {
	files := migrationFilesForLint(t, "*.up.sql")

	// Migrations through 128 contain historical duplicate numeric prefixes.
	// From 129 onward, keep the numeric sequence unique so release tooling and
	// operators can identify one schema change unambiguously by its number.
	const firstUniqueMigrationNumber = 129
	stemByNumber := make(map[int]string)
	for _, file := range files {
		stem, _, ok := splitMigrationFilename(filepath.Base(file))
		if !ok {
			continue
		}
		prefix, _, ok := strings.Cut(stem, "_")
		if !ok {
			continue
		}
		number, err := strconv.Atoi(prefix)
		if err != nil || number < firstUniqueMigrationNumber {
			continue
		}
		if _, deployed := forkDuplicateMigrationStems[stem]; deployed {
			continue
		}
		if previous, exists := stemByNumber[number]; exists {
			t.Errorf("migrations %s and %s share numeric prefix %s", previous, stem, prefix)
			continue
		}
		stemByNumber[number] = stem
	}
}

func TestMigrationFilesHaveMatchingDirections(t *testing.T) {
	files := migrationFilesForLint(t, "*.sql")

	directionsByStem := make(map[string]map[string]bool)
	for _, file := range files {
		stem, direction, ok := splitMigrationFilename(filepath.Base(file))
		if !ok {
			continue
		}
		if directionsByStem[stem] == nil {
			directionsByStem[stem] = make(map[string]bool)
		}
		directionsByStem[stem][direction] = true
	}

	for stem, directions := range directionsByStem {
		if !directions["up"] || !directions["down"] {
			t.Errorf("migration %s must have both .up.sql and .down.sql files", stem)
		}
	}
}

func migrationFilesForLint(t *testing.T, pattern string) []string {
	t.Helper()

	dir := realMigrationsDir(t)
	files, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no migration files matched %s in %s", pattern, dir)
	}
	sort.Strings(files)
	return files
}

func realMigrationsDir(t *testing.T) string {
	t.Helper()

	// Relative to the package directory go test runs in; runtime.Caller
	// would return a module path under -trimpath.
	dir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("resolve migrations dir: %v", err)
	}
	return dir
}

func splitMigrationFilename(name string) (stem, direction string, ok bool) {
	for _, candidateDirection := range []string{"up", "down"} {
		suffix := fmt.Sprintf(".%s.sql", candidateDirection)
		if strings.HasSuffix(name, suffix) {
			return strings.TrimSuffix(name, suffix), candidateDirection, true
		}
	}
	return "", "", false
}

// forkDuplicateMigrationStems are fork migrations that were already applied to
// deployed databases before the upstream sync introduced migrations with the
// same numbers. Migrations are tracked by full filename, so renumbering them
// would re-run non-idempotent DDL on those databases. New migrations must not
// be added here.
var forkDuplicateMigrationStems = map[string]struct{}{
		"593_external_agent_identity": {},
		"594_agent_mailbox": {},
		"595_issue_coordinator_merge_policy": {},
		"596_external_agent_session_identity_index": {},
		"597_external_agent_session_agent_index": {},
		"598_external_agent_token_hash_index": {},
		"599_external_agent_token_agent_index": {},
		"600_agent_mailbox_recipient_comment_index": {},
		"601_agent_mailbox_claim_index": {},
		"602_agent_mailbox_recipient_index": {},
		"603_agent_mailbox_issue_index": {},
		"604_agent_mailbox_delivery_core": {},
		"605_agent_mailbox_delivery_idempotency_index": {},
		"606_agent_mailbox_delivery_unknown_status": {},
		"607_agent_mailbox_verify_index": {},
		"608_tgit_provider": {},
		"610_tgit_snapshot_refresh_index": {},
		"611_external_agent_session_binding": {},
		"612_agent_mailbox_frozen_target": {},
		"613_drop_external_agent_session_agent_unique": {},
		"614_agent_session_binding_active_index": {},
		"615_agent_mailbox_notification_key_index": {},
		"616_agent_mailbox_target_session_index": {},
		"617_drop_mailbox_source_key_unique": {},
		"618_issue_delegation_return": {},
		"619_issue_delegation_return_issue_index": {},
		"620_agent_task_delegation_return": {},
		"621_comment_dispatch_obligation": {},
		"622_comment_dispatch_obligation_target_index": {},
		"623_comment_dispatch_obligation_pending_index": {},
		"630_agent_mailbox_withdrawn_at": {},
		"631_delegation_result_obligations": {},
		"632_delegation_result_task_index": {},
		"633_task_session_pin_confirmation": {},
		"634_task_session_pin_generation": {},
		"635_delegation_original_task_scope": {},
		"636_ordinary_task_pending_thread_index": {},
		"637_retire_shared_task_pending_thread_index": {},
		"638_delegation_result_attempt_time": {},
		"639_result_takeover": {},
		"640_current_delegation_return_index": {},
		"641_retire_delegation_return_dispatch_index": {},
		"642_result_takeover_request_index": {},
		"643_result_takeover_selection_version": {},
}
