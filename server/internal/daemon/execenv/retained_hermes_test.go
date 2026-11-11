package execenv

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStrictHermesReuseKeepsTranscriptWhenSkillsRemoved(t *testing.T) {
	requireSymlinks(t)
	home := t.TempDir()
	mustWrite(t, filepath.Join(home, "config.yaml"), "model: current-model\n")
	memory, session := t.TempDir(), t.TempDir()
	task := TaskContextForEnv{AgentID: "agent", IssueID: "issue", AgentSkills: []SkillContextForEnv{{Name: "old-skill", Content: "old instructions"}}}
	env, err := Prepare(PrepareParams{WorkspacesRoot: t.TempDir(), WorkspaceID: "workspace", TaskID: "source", Provider: "hermes", HermesSourceHome: home, HermesMemoryStore: memory, HermesSessionStore: session, Task: task}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer env.Cleanup(true)
	mustWrite(t, filepath.Join(env.HermesHome, "state.db"), "original transcript")
	original, err := os.Stat(filepath.Join(env.HermesHome, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	task.AgentSkills = nil
	task.AgentName = "current return identity"
	reused := Reuse(ReuseParams{RequireResume: true, RootDir: env.RootDir, WorkDir: env.WorkDir, Provider: "hermes", HermesSourceHome: home, HermesMemoryStore: memory, HermesSessionStore: session, Task: task}, testLogger())
	if reused == nil {
		t.Fatal("strict reuse discarded an existing transcript when last skill was removed")
	}
	current, err := os.Stat(filepath.Join(reused.HermesHome, "state.db"))
	if err != nil || !os.SameFile(original, current) {
		t.Fatalf("session store identity changed: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(reused.HermesHome, "state.db"))
	if err != nil || string(data) != "original transcript" {
		t.Fatalf("transcript changed: %s %v", data, err)
	}
	config, err := os.ReadFile(filepath.Join(reused.HermesHome, "config.yaml"))
	if err != nil || len(config) == 0 {
		t.Fatalf("current config was not refreshed: %v", err)
	}
}

func TestStrictHermesReuseRejectsChangedProfileWithoutRemovingOldSession(t *testing.T) {
	requireSymlinks(t)
	home := t.TempDir()
	memory, session := t.TempDir(), t.TempDir()
	task := TaskContextForEnv{AgentID: "agent", IssueID: "issue", AgentSkills: []SkillContextForEnv{{Name: "skill", Content: "instructions"}}}
	env, err := Prepare(PrepareParams{WorkspacesRoot: t.TempDir(), WorkspaceID: "workspace", TaskID: "source", Provider: "hermes", HermesSourceHome: home, HermesMemoryStore: memory, HermesSessionStore: session, Task: task}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer env.Cleanup(true)
	mustWrite(t, filepath.Join(env.HermesHome, "state.db"), "original transcript")
	otherSession := t.TempDir()
	mustWrite(t, filepath.Join(otherSession, "state.db"), "different transcript")
	reused := Reuse(ReuseParams{RequireResume: true, RootDir: env.RootDir, WorkDir: env.WorkDir, Provider: "hermes", HermesSourceHome: t.TempDir(), HermesMemoryStore: t.TempDir(), HermesSessionStore: otherSession, Task: task}, testLogger())
	if reused != nil {
		t.Fatal("strict resume accepted another profile store")
	}
	data, err := os.ReadFile(filepath.Join(env.HermesHome, "state.db"))
	if err != nil || string(data) != "original transcript" {
		t.Fatalf("rejected reuse changed the old session: %s %v", data, err)
	}
}

func TestStrictHermesTaskScopeKeepsTaskLocalState(t *testing.T) {
	requireSymlinks(t)
	home, memory := t.TempDir(), t.TempDir()
	task := TaskContextForEnv{AgentID: "agent", ScopeTaskID: "original-task-S", AgentSkills: []SkillContextForEnv{{Name: "skill", Content: "instructions"}}}
	env, err := Prepare(PrepareParams{WorkspacesRoot: t.TempDir(), WorkspaceID: "workspace", TaskID: "original-task-S", Provider: "hermes", HermesSourceHome: home, HermesMemoryStore: memory, Task: task}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer env.Cleanup(true)
	state := filepath.Join(env.HermesHome, "state.db")
	mustWrite(t, state, "original task transcript")
	before, err := os.Stat(state)
	if err != nil {
		t.Fatal(err)
	}
	task.AgentSkills = nil
	params := ReuseParams{RequireResume: true, RootDir: env.RootDir, WorkDir: env.WorkDir, Provider: "hermes", HermesSourceHome: home, HermesMemoryStore: memory, Task: task}
	reused := Reuse(params, testLogger())
	if reused == nil || reused.HermesSessionStore != "" {
		t.Fatal("task-local state was moved into a handling-task store")
	}
	after, err := os.Stat(state)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("task session identity changed: %v", err)
	}
	if err := os.Remove(filepath.Join(env.HermesHome, hermesTaskLocalStateMarker)); err != nil {
		t.Fatal(err)
	}
	if got := Reuse(params, testLogger()); got != nil {
		t.Fatal("strict reuse ran an unproven state migration")
	}
	data, err := os.ReadFile(state)
	if err != nil || string(data) != "original task transcript" {
		t.Fatalf("unknown state migration erased source session: %q %v", data, err)
	}
}
