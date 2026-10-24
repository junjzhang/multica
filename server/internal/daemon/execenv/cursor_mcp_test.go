package execenv

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCursorMcpApprovalKeyMatchesCursorAgent(t *testing.T) {
	t.Parallel()

	keys, err := cursorMcpApprovalKeys("/tmp/work", map[string]json.RawMessage{
		"fetch": json.RawMessage(`{"command":"uvx","args":["mcp-server-fetch"]}`),
	})
	if err != nil {
		t.Fatalf("cursorMcpApprovalKeys: %v", err)
	}
	want := []string{"fetch-b3a6127d3cbd8e52"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("approval keys = %v, want %v", keys, want)
	}
}

func TestCursorMcpApprovalKeyMatchesCursorAgentWithReorderedStdioConfig(t *testing.T) {
	t.Parallel()

	keys, err := cursorMcpApprovalKeys("/tmp/work", map[string]json.RawMessage{
		"fetch": json.RawMessage(`{ "args": [ "mcp-server-fetch" ], "command": "uvx", "env": { "TOKEN": "x" } }`),
	})
	if err != nil {
		t.Fatalf("cursorMcpApprovalKeys: %v", err)
	}
	want := []string{"fetch-4c7232835349773b"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("approval keys = %v, want %v", keys, want)
	}
}

func TestCursorMcpApprovalKeyMatchesJSONStringifyWithHTMLCharacters(t *testing.T) {
	t.Parallel()

	keys, err := cursorMcpApprovalKeys("/tmp/work", map[string]json.RawMessage{
		"fetch": json.RawMessage(`{"command":"uvx","args":["mcp"],"env":{"URL":"https://x.test?a=1&b=2"}}`),
	})
	if err != nil {
		t.Fatalf("cursorMcpApprovalKeys: %v", err)
	}
	want := []string{"fetch-37ab92a3e088d73a"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("approval keys = %v, want %v", keys, want)
	}
}

func TestCursorMcpApprovalKeyMatchesCursorAgentWithStdioTypeAndCwd(t *testing.T) {
	t.Parallel()

	keys, err := cursorMcpApprovalKeys("/private/tmp/work", map[string]json.RawMessage{
		"withtypecwd": json.RawMessage(`{"enabled":true,"cwd":"/tmp/probe","timeout":30,"args":["mcp"],"command":"uvx","type":"stdio"}`),
	})
	if err != nil {
		t.Fatalf("cursorMcpApprovalKeys: %v", err)
	}
	// Captured from cursor-agent 2026.08.04-aaa8809 via mcp enable.
	want := []string{"withtypecwd-ba71d2f70913528e"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("approval keys = %v, want %v", keys, want)
	}
}

func TestCursorMcpApprovalKeyMatchesCursorAgentForRemoteServer(t *testing.T) {
	t.Parallel()

	keys, err := cursorMcpApprovalKeys("/private/tmp/work", map[string]json.RawMessage{
		"remote": json.RawMessage(`{"enabled":true,"headers":{"Authorization":"Bearer x"},"timeout":30,"url":"https://mcp.example.com","type":"http"}`),
	})
	if err != nil {
		t.Fatalf("cursorMcpApprovalKeys: %v", err)
	}
	// Captured from cursor-agent 2026.08.04-aaa8809 via mcp enable.
	want := []string{"remote-abc2ffe78532b37e"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("approval keys = %v, want %v", keys, want)
	}
}

// cursorMcpTestRepo gives Cursor MCP preparation an explicit Git root inside
// t.TempDir(). The resolver walks up to the nearest ancestor containing .git;
// without this boundary a bare t.TempDir() can resolve to a host /tmp/.git or
// ~/.git and write outside the test directory. WorkDir is nested below the
// root so the fixture also exercises ancestor lookup.
type cursorMcpTestRepo struct {
	TempRoot string
	RepoRoot string
	WorkDir  string
}

func newCursorMcpTestRepo(t *testing.T) cursorMcpTestRepo {
	t.Helper()

	tempRoot := t.TempDir()
	repoRoot := filepath.Join(tempRoot, "repo")
	workDir := filepath.Join(repoRoot, "workdir")
	for _, dir := range []string{workDir, filepath.Join(repoRoot, ".git")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	return cursorMcpTestRepo{TempRoot: tempRoot, RepoRoot: repoRoot, WorkDir: workDir}
}

func (r cursorMcpTestRepo) projectRoot(t *testing.T) string {
	t.Helper()

	want := cursorMcpEvalSymlinks(t, r.RepoRoot)
	got := cursorProjectRoot(r.WorkDir)
	if got != want {
		t.Fatalf("cursorProjectRoot(%q) = %q, want explicit Git root %q", r.WorkDir, got, want)
	}
	requireCursorMcpPathWithin(t, r.TempRoot, got)
	return got
}

func cursorMcpEvalSymlinks(t *testing.T, path string) string {
	t.Helper()

	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("eval symlinks %s: %v", path, err)
	}
	return resolved
}

func requireCursorMcpPathWithin(t *testing.T, root, path string) {
	t.Helper()

	resolvedRoot := cursorMcpEvalSymlinks(t, root)
	resolvedPath := cursorMcpEvalSymlinks(t, path)
	rel, err := filepath.Rel(resolvedRoot, resolvedPath)
	if err != nil {
		t.Fatalf("relative path from %s to %s: %v", resolvedRoot, resolvedPath, err)
	}
	if rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("path %s escapes temporary root %s (rel %q)", resolvedPath, resolvedRoot, rel)
	}
}

func TestPrepareCursorMcpConfigWritesProjectConfigAndApprovals(t *testing.T) {
	t.Parallel()

	repo := newCursorMcpTestRepo(t)
	envRoot := repo.TempRoot
	workDir := repo.WorkDir
	manifest := &sidecarManifest{}
	mcpConfig := json.RawMessage(`{
		"mcpServers": {
			"fetch": {"command":"uvx","args":["mcp-server-fetch"]},
			"http": {"url":"https://mcp.example.com","type":"http"}
		}
	}`)

	cursorDataDir, err := prepareCursorMcpConfig(envRoot, workDir, mcpConfig, "", manifest)
	if err != nil {
		t.Fatalf("prepareCursorMcpConfig: %v", err)
	}
	if cursorDataDir != filepath.Join(envRoot, "cursor-data") {
		t.Fatalf("CursorDataDir = %q, want envRoot/cursor-data", cursorDataDir)
	}

	projectRoot := repo.projectRoot(t)
	configPath := filepath.Join(projectRoot, ".cursor", "mcp.json")
	rawConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read .cursor/mcp.json: %v", err)
	}
	var cfg cursorMcpConfigFile
	if err := json.Unmarshal(rawConfig, &cfg); err != nil {
		t.Fatalf("unmarshal .cursor/mcp.json: %v\n%s", err, rawConfig)
	}
	if len(cfg.McpServers) != 2 {
		t.Fatalf("mcpServers length = %d, want 2: %s", len(cfg.McpServers), rawConfig)
	}
	if mode := filePerm(t, configPath); mode != 0o600 {
		t.Fatalf(".cursor/mcp.json mode = %#o, want 0600", mode)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".cursor")); !os.IsNotExist(err) {
		t.Fatalf("nested workDir got its own .cursor, stat err=%v", err)
	}

	projectDataDir := filepath.Join(cursorDataDir, "projects", cursorSlugifyPath(projectRoot))
	rawApprovals, err := os.ReadFile(filepath.Join(projectDataDir, "mcp-approvals.json"))
	if err != nil {
		t.Fatalf("read mcp-approvals.json: %v", err)
	}
	var approvals []string
	if err := json.Unmarshal(rawApprovals, &approvals); err != nil {
		t.Fatalf("unmarshal mcp-approvals.json: %v\n%s", err, rawApprovals)
	}
	wantApprovals, err := cursorMcpApprovalKeys(projectRoot, cfg.McpServers)
	if err != nil {
		t.Fatalf("expected approvals: %v", err)
	}
	if !reflect.DeepEqual(approvals, wantApprovals) {
		t.Fatalf("approvals = %v, want %v", approvals, wantApprovals)
	}
	if mode := filePerm(t, filepath.Join(projectDataDir, "mcp-approvals.json")); mode != 0o600 {
		t.Fatalf("mcp-approvals.json mode = %#o, want 0600", mode)
	}
	if _, err := os.Stat(filepath.Join(projectDataDir, cursorWorkspaceTrustedFile)); err != nil {
		t.Fatalf("workspace trust file missing: %v", err)
	}
	requireCursorMcpPathWithin(t, envRoot, projectDataDir)
	if len(manifest.Files) == 0 {
		t.Fatal("manifest did not record .cursor/mcp.json")
	}
}

func TestPrepareCursorMcpConfigManagedEmptySet(t *testing.T) {
	t.Parallel()

	repo := newCursorMcpTestRepo(t)
	envRoot := repo.TempRoot
	workDir := repo.WorkDir
	cursorDataDir, err := prepareCursorMcpConfig(envRoot, workDir, json.RawMessage(`{"mcpServers":{}}`), "", &sidecarManifest{})
	if err != nil {
		t.Fatalf("prepareCursorMcpConfig: %v", err)
	}
	if cursorDataDir == "" {
		t.Fatal("managed empty mcp_config should still isolate CursorDataDir")
	}
	projectRoot := repo.projectRoot(t)
	approvalsPath := filepath.Join(cursorDataDir, "projects", cursorSlugifyPath(projectRoot), "mcp-approvals.json")
	rawApprovals, err := os.ReadFile(approvalsPath)
	if err != nil {
		t.Fatalf("read mcp-approvals.json: %v", err)
	}
	var approvals []string
	if err := json.Unmarshal(rawApprovals, &approvals); err != nil {
		t.Fatalf("unmarshal approvals: %v", err)
	}
	if len(approvals) != 0 {
		t.Fatalf("approvals length = %d, want 0: %v", len(approvals), approvals)
	}
	requireCursorMcpPathWithin(t, envRoot, approvalsPath)
}

func TestPrepareCursorMcpConfigSeedsExplicitAuthSource(t *testing.T) {
	t.Parallel()

	repo := newCursorMcpTestRepo(t)
	envRoot := repo.TempRoot
	workDir := repo.WorkDir
	sourceProjectDir := filepath.Join(envRoot, "source-project")
	if err := os.MkdirAll(sourceProjectDir, 0o700); err != nil {
		t.Fatalf("mkdir source project: %v", err)
	}
	sourceAuth := filepath.Join(sourceProjectDir, cursorMcpAuthFile)
	if err := os.WriteFile(sourceAuth, []byte(`{"tokens":{"fetch":"secret"}}`), 0o600); err != nil {
		t.Fatalf("write source auth: %v", err)
	}

	cursorDataDir, err := prepareCursorMcpConfig(envRoot, workDir, json.RawMessage(`{"mcpServers":{"fetch":{"command":"uvx","args":["mcp-server-fetch"]}}}`), sourceProjectDir, &sidecarManifest{})
	if err != nil {
		t.Fatalf("prepareCursorMcpConfig: %v", err)
	}
	projectRoot := repo.projectRoot(t)
	targetAuth := filepath.Join(cursorDataDir, "projects", cursorSlugifyPath(projectRoot), cursorMcpAuthFile)
	data, err := os.ReadFile(targetAuth)
	if err != nil {
		t.Fatalf("read seeded auth: %v", err)
	}
	if string(data) != `{"tokens":{"fetch":"secret"}}` {
		t.Fatalf("seeded auth = %s", data)
	}
	requireCursorMcpPathWithin(t, envRoot, targetAuth)
}

func TestPrepareCursorMcpConfigRemovesPriorAuthOnOptOut(t *testing.T) {
	t.Parallel()

	repo := newCursorMcpTestRepo(t)
	envRoot := repo.TempRoot
	workDir := repo.WorkDir
	sourceProjectDir := filepath.Join(envRoot, "source-project")
	if err := os.MkdirAll(sourceProjectDir, 0o700); err != nil {
		t.Fatalf("mkdir source project: %v", err)
	}
	sourceAuth := filepath.Join(sourceProjectDir, cursorMcpAuthFile)
	if err := os.WriteFile(sourceAuth, []byte(`{"tokens":{"fetch":"secret"}}`), 0o600); err != nil {
		t.Fatalf("write source auth: %v", err)
	}
	mcpConfig := json.RawMessage(`{"mcpServers":{"fetch":{"command":"uvx","args":["mcp-server-fetch"]}}}`)

	cursorDataDir, err := prepareCursorMcpConfig(envRoot, workDir, mcpConfig, sourceProjectDir, nil)
	if err != nil {
		t.Fatalf("prepareCursorMcpConfig with auth source: %v", err)
	}
	projectRoot := repo.projectRoot(t)
	targetAuth := filepath.Join(cursorDataDir, "projects", cursorSlugifyPath(projectRoot), cursorMcpAuthFile)
	if _, err := os.Stat(targetAuth); err != nil {
		t.Fatalf("seeded auth missing: %v", err)
	}

	cursorDataDir, err = prepareCursorMcpConfig(envRoot, workDir, mcpConfig, "", nil)
	if err != nil {
		t.Fatalf("prepareCursorMcpConfig without auth source: %v", err)
	}
	targetAuth = filepath.Join(cursorDataDir, "projects", cursorSlugifyPath(projectRoot), cursorMcpAuthFile)
	if _, err := os.Stat(targetAuth); !os.IsNotExist(err) {
		t.Fatalf("auth file should be removed after opt-out, stat err=%v", err)
	}
	requireCursorMcpPathWithin(t, envRoot, filepath.Dir(targetAuth))
}

func TestPrepareCursorMcpConfigRejectsArbitraryAuthSourceFile(t *testing.T) {
	t.Parallel()

	repo := newCursorMcpTestRepo(t)
	envRoot := repo.TempRoot
	workDir := repo.WorkDir
	source := filepath.Join(envRoot, "other.json")
	if err := os.WriteFile(source, []byte(`{"secret":true}`), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}

	_, err := prepareCursorMcpConfig(envRoot, workDir, json.RawMessage(`{"mcpServers":{}}`), source, &sidecarManifest{})
	if err == nil {
		t.Fatal("expected non-mcp-auth source file to fail")
	}

	// Preparation writes the managed config before it validates the auth
	// source, so an error path must stay inside the fixture too.
	projectRoot := repo.projectRoot(t)
	requireCursorMcpPathWithin(t, envRoot, filepath.Join(projectRoot, ".cursor", "mcp.json"))
	if _, err := os.Stat(filepath.Join(workDir, ".cursor")); !os.IsNotExist(err) {
		t.Fatalf("nested workDir got its own .cursor, stat err=%v", err)
	}
}

func TestPrepareCursorMcpConfigNilDoesNotTakeOwnership(t *testing.T) {
	t.Parallel()

	repo := newCursorMcpTestRepo(t)
	envRoot := repo.TempRoot
	workDir := repo.WorkDir
	cursorDataDir, err := prepareCursorMcpConfig(envRoot, workDir, nil, "", &sidecarManifest{})
	if err != nil {
		t.Fatalf("prepareCursorMcpConfig: %v", err)
	}
	if cursorDataDir != "" {
		t.Fatalf("CursorDataDir = %q, want empty", cursorDataDir)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".cursor", "mcp.json")); !os.IsNotExist(err) {
		t.Fatalf(".cursor/mcp.json should not exist, stat err=%v", err)
	}
}

func TestPrepareCursorMcpConfigRejectsMalformedConfig(t *testing.T) {
	t.Parallel()

	repo := newCursorMcpTestRepo(t)
	envRoot := repo.TempRoot
	workDir := repo.WorkDir
	_, err := prepareCursorMcpConfig(envRoot, workDir, json.RawMessage(`{"mcpServers":{"bad":42}}`), "", &sidecarManifest{})
	if err == nil {
		t.Fatal("expected malformed server config to fail")
	}
}

// A .git above the fixture reproduces the escaping shape: without a local Git
// root the resolver climbs to it. With a temporary root in place, preparation
// stops at the nearest root and cannot write outside the fixture.
func TestPrepareCursorMcpConfigStopsAtNearestTemporaryGitRoot(t *testing.T) {
	t.Parallel()

	tempRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tempRoot, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir simulated ancestor .git: %v", err)
	}
	envRoot := filepath.Join(tempRoot, "env")
	repoRoot := filepath.Join(envRoot, "repo")
	workDir := filepath.Join(repoRoot, "nested", "workdir")
	for _, dir := range []string{workDir, filepath.Join(repoRoot, ".git")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	unrooted := filepath.Join(envRoot, "unrooted")
	if err := os.MkdirAll(unrooted, 0o755); err != nil {
		t.Fatalf("mkdir unrooted: %v", err)
	}

	wantAncestor := cursorMcpEvalSymlinks(t, tempRoot)
	if got := cursorProjectRoot(unrooted); got != wantAncestor {
		t.Fatalf("cursorProjectRoot without a local .git = %q, want simulated ancestor %q", got, wantAncestor)
	}

	cursorDataDir, err := prepareCursorMcpConfig(
		envRoot,
		workDir,
		json.RawMessage(`{"mcpServers":{"fetch":{"command":"uvx","args":["mcp-server-fetch"]}}}`),
		"",
		&sidecarManifest{},
	)
	if err != nil {
		t.Fatalf("prepareCursorMcpConfig: %v", err)
	}
	projectRoot := cursorProjectRoot(workDir)
	wantRepo := cursorMcpEvalSymlinks(t, repoRoot)
	if projectRoot != wantRepo {
		t.Fatalf("cursorProjectRoot(%q) = %q, want nearest temporary root %q", workDir, projectRoot, wantRepo)
	}

	configPath := filepath.Join(projectRoot, ".cursor", "mcp.json")
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("managed config not written under nearest Git root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tempRoot, ".cursor")); !os.IsNotExist(err) {
		t.Fatalf("preparation escaped to simulated ancestor .cursor, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".cursor")); !os.IsNotExist(err) {
		t.Fatalf("preparation wrote into nested workDir, stat err=%v", err)
	}
	requireCursorMcpPathWithin(t, tempRoot, configPath)
	requireCursorMcpPathWithin(t, envRoot, cursorDataDir)
}

func filePerm(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}
