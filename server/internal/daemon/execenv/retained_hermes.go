package execenv

import (
	"fmt"
	"os"
	"path/filepath"
)

func validateRetainedHermesStore(params ReuseParams) error {
	root := params.RootDir
	if root == "" {
		root = filepath.Dir(params.WorkDir)
	}
	// A conversation session store is itself the Hermes home; otherwise the
	// task-local overlay under the env root is.
	home := filepath.Join(root, "hermes-home")
	if params.HermesSessionStore != "" {
		home = params.HermesSessionStore
	}
	if info, err := os.Stat(home); err != nil || !info.IsDir() {
		return fmt.Errorf("original Hermes home is unavailable: %s", home)
	}
	marker, markerErr := os.Lstat(filepath.Join(home, hermesTaskLocalStateMarker))
	if markerErr != nil || !marker.Mode().IsRegular() {
		return fmt.Errorf("original Hermes state ownership cannot be proven: %s", home)
	}
	for entry, store := range map[string]string{"state.db": params.HermesSessionStore, "memories": params.HermesMemoryStore} {
		if store == "" && entry == "state.db" && params.Task.ScopeTaskID != "" {
			info, err := os.Lstat(filepath.Join(home, entry))
			if err != nil || !info.Mode().IsRegular() {
				return fmt.Errorf("original task-local Hermes transcript cannot be proven: %s", home)
			}
			continue
		}
		if store == "" {
			return fmt.Errorf("original Hermes %s store cannot be proven", entry)
		}
		current, err := os.Stat(filepath.Join(home, entry))
		if err != nil {
			return err
		}
		expectedPath := store
		if entry == "state.db" {
			expectedPath = filepath.Join(store, entry)
		}
		expected, err := os.Stat(expectedPath)
		if err != nil {
			return err
		}
		if !os.SameFile(current, expected) {
			return fmt.Errorf("Hermes %s store differs from the retained environment", entry)
		}
	}
	return nil
}
