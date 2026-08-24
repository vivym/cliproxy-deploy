package configcli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type configurationMaintenanceBoundary struct {
	sessionPath string
	lockPath    string
	modePath    string
}

func acquireConfigurationMaintenanceBoundary(root string) (*configurationMaintenanceBoundary, error) {
	opsPath, err := safeArtifactPath(root, "ops")
	if err != nil {
		return nil, fmt.Errorf("resolve configuration maintenance directory: %w", err)
	}
	if err := ensureSafeParent(root, opsPath); err != nil {
		return nil, fmt.Errorf("prepare configuration maintenance directory: %w", err)
	}
	boundary := &configurationMaintenanceBoundary{
		sessionPath: filepath.Join(opsPath, "maintenance.session"),
		lockPath:    filepath.Join(opsPath, "maintenance.lock"),
	}
	boundary.modePath = filepath.Join(boundary.lockPath, "mode")
	if err := os.Mkdir(boundary.sessionPath, 0o700); err != nil {
		return nil, errors.New("another deployment maintenance session is active")
	}
	if err := os.Mkdir(boundary.lockPath, 0o700); err != nil {
		_ = os.Remove(boundary.sessionPath)
		return nil, errors.New("another deployment maintenance lock is active")
	}
	modeFile, err := os.OpenFile(boundary.modePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		var written int
		written, err = modeFile.WriteString("config\n")
		if err == nil && written != len("config\n") {
			err = io.ErrShortWrite
		}
		if err == nil {
			err = modeFile.Sync()
		}
	}
	if modeFile != nil {
		if closeErr := modeFile.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		_ = os.Remove(boundary.modePath)
		_ = os.Remove(boundary.lockPath)
		_ = os.Remove(boundary.sessionPath)
		return nil, errors.New("create configuration maintenance mode failed")
	}
	return boundary, nil
}

func (boundary *configurationMaintenanceBoundary) Verify() error {
	if boundary == nil {
		return errors.New("configuration maintenance boundary is required")
	}
	contents, exists, err := readRegularFile(boundary.modePath)
	if err != nil || !exists || string(contents) != "config\n" {
		return errors.New("configuration maintenance boundary is no longer owned")
	}
	return nil
}

func (boundary *configurationMaintenanceBoundary) Release() error {
	if err := boundary.Verify(); err != nil {
		return err
	}
	if err := os.Remove(boundary.modePath); err != nil {
		return err
	}
	if err := os.Remove(boundary.lockPath); err != nil {
		return err
	}
	if err := os.Remove(boundary.sessionPath); err != nil {
		return err
	}
	return nil
}
