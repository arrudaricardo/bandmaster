package project

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func writeFile(path string, content []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".bandmaster-write-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func validateLocalPath(root, relative string) *Error {
	current := root
	parts := strings.Split(filepath.Clean(relative), string(filepath.Separator))
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return internal("inspect project artifact path", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return invalid("unsafe_project_path", fmt.Sprintf("Project artifact path %q traverses a symlink.", filepath.ToSlash(relative)))
		}
		if index < len(parts)-1 && !info.IsDir() {
			return invalid("unsafe_project_path", fmt.Sprintf("Project artifact parent %q is not a directory.", filepath.ToSlash(current)))
		}
		if index == len(parts)-1 && !info.Mode().IsRegular() {
			return invalid("unsafe_project_path", fmt.Sprintf("Project artifact path %q is not a regular file.", filepath.ToSlash(relative)))
		}
	}
	return nil
}
