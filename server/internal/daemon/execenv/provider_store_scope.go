package execenv

import (
	"path/filepath"
	"strings"

	"github.com/multica-ai/multica/server/internal/cli"
)

// ProviderStoreScope derives only the existing GC ownership group. It is not
// a provider session address and must never be used to select a return turn.
func ProviderStoreScope(profile, provider, store string) (agentID, scopeType, scopeID string, ok bool) {
	var root string
	switch provider {
	case "codex":
		root = filepath.Join(resolveSharedCodexHome(), codexSessionStoreRoot, codexSessionStoreNamespace(profile))
	case "hermes":
		profileDir, err := cli.ProfileDir(profile)
		if err != nil {
			return
		}
		for _, kind := range []string{hermesMemoryStoreRoot, hermesSessionStoreRoot} {
			candidate := filepath.Join(profileDir, kind)
			rel, err := filepath.Rel(candidate, store)
			if err != nil || !filepath.IsLocal(rel) {
				continue
			}
			parts := strings.Split(rel, string(filepath.Separator))
			if kind == hermesMemoryStoreRoot && len(parts) == 2 && parts[0] != "" {
				return parts[0], "agent", "", true
			}
			if kind == hermesSessionStoreRoot && len(parts) == 3 && parts[0] != "" {
				return storeScope(parts[0], parts[2])
			}
		}
		return
	default:
		return
	}
	rel, err := filepath.Rel(root, store)
	if err != nil || !filepath.IsLocal(rel) {
		return
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 2 {
		return
	}
	return storeScope(parts[0], parts[1])
}

func storeScope(agentID, conversation string) (string, string, string, bool) {
	if agentID == "" || agentID == "_" || conversation == "" {
		return "", "", "", false
	}
	if strings.HasPrefix(conversation, "chat_") {
		id := strings.TrimPrefix(conversation, "chat_")
		return agentID, "chat", id, id != ""
	}
	return agentID, "issue", conversation, true
}
