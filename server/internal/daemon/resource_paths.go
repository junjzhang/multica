package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func (d *Daemon) permitResourceRelease(request protocol.ResourceReleaseRequest, decision protocol.ResourceReleaseDecision) bool {
	if !decision.ReleaseAllowed {
		return false
	}
	if request.Kind != "provider_store" && decision.ControlRequired && !decision.AdmissionClosed {
		return false
	}
	if request.Kind != "workdir" {
		return true
	}
	for _, address := range decision.ProtectedResources {
		if address.WorkspaceID == "" || address.WorkspaceID != request.WorkspaceID || address.SourceTaskID == "" || address.AgentID == "" || address.RuntimeID == "" || address.Provider == "" || address.WorkDir == "" {
			d.logger.Warn("retaining resource: protected address identity is incomplete or outside its workspace", "resource", request.ResourcePath)
			return false
		}
		overlaps, err := physicalResourceOverlap(request.ResourcePath, address.WorkDir)
		if err != nil || overlaps {
			d.logger.Info("retaining resource for protected physical address", "resource", request.ResourcePath, "source_task", address.SourceTaskID, "error", err)
			return false
		}
	}
	return true
}

func physicalResourceOverlap(root, target string) (bool, error) {
	root, err := resolveResourcePath(root)
	if err != nil {
		return false, err
	}
	target, err = resolveResourcePath(target)
	if err != nil {
		return false, err
	}
	if pathContains(root, target) || pathContains(target, root) {
		return true, nil
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		return false, err
	}
	if found, err := ancestorMatches(target, rootInfo); err != nil || found {
		return found, err
	}
	targetInfo, err := os.Stat(target)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return ancestorMatches(root, targetInfo)
}

func pathContains(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	return err == nil && filepath.IsLocal(rel)
}

// Missing planned directories can be resolved through an existing ancestor;
// dangling links and inaccessible ancestors cannot establish a safe address.
func resolveResourcePath(name string) (string, error) {
	if !filepath.IsAbs(name) {
		return "", fmt.Errorf("resource path is not absolute: %s", name)
	}
	current := filepath.Clean(name)
	var suffix []string
	for {
		_, err := os.Stat(current)
		if err == nil {
			resolved, err := util.ResolveSymlinks(current)
			if err != nil {
				return "", err
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if _, linkErr := os.Lstat(current); linkErr == nil {
			return "", fmt.Errorf("resource has an unresolved link: %s", current)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func ancestorMatches(name string, want os.FileInfo) (bool, error) {
	for {
		info, err := os.Stat(name)
		if err == nil && os.SameFile(info, want) {
			return true, nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		parent := filepath.Dir(name)
		if parent == name {
			return false, nil
		}
		name = parent
	}
}
