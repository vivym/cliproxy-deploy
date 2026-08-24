package configcli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/tenantconfig"
)

type localExecutor struct {
	root string
}

func (executor *localExecutor) Execute(_ context.Context, change tenantconfig.Change) (tenantconfig.ExecutionResult, error) {
	if change.Target != tenantconfig.TargetLocal || change.Action != tenantconfig.ActionWriteArtifact {
		return tenantconfig.ExecutionResult{}, errors.New("remote operation requires an explicit remote executor")
	}
	path, err := safeArtifactPath(executor.root, change.Resource)
	if err != nil {
		return tenantconfig.ExecutionResult{}, err
	}
	current, exists, err := readRegularFile(path)
	if err != nil {
		return tenantconfig.ExecutionResult{}, err
	}
	if exists {
		currentDigest := sha256Hex(current)
		if currentDigest == change.DesiredDigest {
			return tenantconfig.ExecutionResult{ResultDigest: currentDigest, Replayed: true}, nil
		}
		if change.BeforeDigest == "" || currentDigest != change.BeforeDigest {
			return tenantconfig.ExecutionResult{}, errors.New("local artifact changed after planning")
		}
	} else if change.BeforeDigest != "" {
		return tenantconfig.ExecutionResult{}, errors.New("local artifact disappeared after planning")
	}
	if err := ensureSafeParent(executor.root, filepath.Dir(path)); err != nil {
		return tenantconfig.ExecutionResult{}, err
	}
	if err := atomicWrite(path, change.Payload, 0o644); err != nil {
		return tenantconfig.ExecutionResult{}, err
	}
	return tenantconfig.ExecutionResult{ResultDigest: change.DesiredDigest}, nil
}

func safeArtifactPath(root, artifactPath string) (string, error) {
	if root == "" || artifactPath == "" || filepath.IsAbs(artifactPath) {
		return "", errors.New("artifact root and relative path are required")
	}
	clean := filepath.Clean(filepath.FromSlash(artifactPath))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("artifact path escapes the output root")
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	return filepath.Join(absoluteRoot, clean), nil
}

func ensureSafeParent(root, parent string) error {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(absoluteRoot, 0o755); err != nil {
		return err
	}
	rootInfo, err := os.Lstat(absoluteRoot)
	if err != nil {
		return err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return errors.New("artifact root contains a symlink or is not a directory")
	}
	relative, err := filepath.Rel(absoluteRoot, parent)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("artifact parent escapes the output root")
	}
	current := absoluteRoot
	parts := strings.Split(relative, string(filepath.Separator))
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("artifact parent contains a symlink or non-directory")
		}
	}
	return nil
}

func atomicWriteWithin(root, path string, contents []byte, mode os.FileMode) error {
	absolutePath, err := managedPath(root, path)
	if err != nil {
		return err
	}
	if err := ensureSafeParent(root, filepath.Dir(absolutePath)); err != nil {
		return err
	}
	return atomicWrite(absolutePath, contents, mode)
}

func managedPath(root, path string) (string, error) {
	if root == "" || path == "" {
		return "", errors.New("configuration root and path are required")
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(absoluteRoot, absolutePath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("configuration path escapes its managed root")
	}
	return absolutePath, nil
}

func preflightManagedOutputPath(root, path string, mode os.FileMode) (string, error) {
	absolutePath, err := managedPath(root, path)
	if err != nil {
		return "", err
	}
	parent := filepath.Dir(absolutePath)
	if err := ensureSafeParent(root, parent); err != nil {
		return "", err
	}
	if _, _, err := readRegularFile(absolutePath); err != nil {
		return "", err
	}
	probe, err := os.CreateTemp(parent, ".lark-config-receipt-probe-*")
	if err != nil {
		return "", err
	}
	probePath := probe.Name()
	if err := probe.Chmod(mode); err != nil {
		_ = probe.Close()
		_ = os.Remove(probePath)
		return "", err
	}
	if err := probe.Close(); err != nil {
		_ = os.Remove(probePath)
		return "", err
	}
	if err := os.Remove(probePath); err != nil {
		return "", err
	}
	return absolutePath, nil
}

func sameManagedFile(left, right string) bool {
	if left == right {
		return true
	}
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	return leftErr == nil && rightErr == nil && os.SameFile(leftInfo, rightInfo)
}

func readRegularFile(path string) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, false, errors.New("path is not a regular non-symlink file")
	}
	if info.Size() > 16<<20 {
		return nil, false, errors.New("file exceeds the 16 MiB configuration limit")
	}
	contents, err := os.ReadFile(path)
	return contents, true, err
}

func atomicWrite(path string, contents []byte, mode os.FileMode) error {
	if len(contents) > 16<<20 {
		return errors.New("configuration output exceeds 16 MiB")
	}
	parent := filepath.Dir(path)
	if err := ensureWriteParent(parent); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing to replace a symlink")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temporary, err := os.CreateTemp(parent, ".lark-config-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	directory, err := os.Open(parent)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func ensureWriteParent(parent string) error {
	absoluteParent, err := filepath.Abs(parent)
	if err != nil {
		return err
	}
	existing := absoluteParent
	for {
		info, statErr := os.Lstat(existing)
		if statErr == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return errors.New("configuration output parent contains a symlink or non-directory")
			}
			return ensureSafeParent(existing, absoluteParent)
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		next := filepath.Dir(existing)
		if next == existing {
			return errors.New("configuration output has no existing parent directory")
		}
		existing = next
	}
}
