package execenv

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ValidateUserWorkDir keeps user-directory writes outside daemon-owned task roots.
// A workdir context marker alone is not ownership: genuine user directories also
// receive one. Task-root owners identify other profiles' managed directories.
func ValidateUserWorkDir(workspacesRoot, workDir string) error {
	resolved, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		return fmt.Errorf("local_directory: resolve working directory: %w", err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return err
	}
	if workspacesRoot != "" {
		root, err := filepath.EvalSymlinks(workspacesRoot)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("local_directory: resolve managed root: %w", err)
		}
		if err == nil {
			root, err = filepath.Abs(root)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, resolved)
			if err == nil && filepath.IsLocal(rel) {
				return fmt.Errorf("local_directory: %q is inside daemon-owned workspaces %q", workDir, root)
			}
		}
	}
	for current := resolved; ; current = filepath.Dir(current) {
		owner, err := ReadEnvRootOwner(current)
		if err != nil {
			return fmt.Errorf("local_directory: cannot verify directory ownership: %w", err)
		}
		if owner.TaskID != "" {
			if owner.WorkspaceID == "" {
				if meta, err := ReadGCMeta(current); err == nil {
					owner.WorkspaceID = strings.TrimSpace(meta.WorkspaceID)
				}
			}
			if err := ValidateEnvRootOwnerPath(filepath.Dir(filepath.Dir(current)), current, *owner); err == nil {
				return fmt.Errorf("local_directory: %q belongs to daemon task %s", workDir, owner.TaskID)
			}
		}
		if filepath.Dir(current) == current {
			return nil
		}
	}
}
