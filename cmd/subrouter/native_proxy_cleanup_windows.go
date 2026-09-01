//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// removePrivateProxyHome removes a child-controlled directory tree using
// handle-relative operations, so a junction inserted during cleanup cannot
// redirect traversal outside the temporary home.
func removePrivateProxyHome(path string) error {
	path = filepath.Clean(path)
	parentPath, name := filepath.Dir(path), filepath.Base(path)
	if name == "." || parentPath == path {
		return fmt.Errorf("refuse to remove private proxy home %q", path)
	}
	parent, err := os.OpenRoot(parentPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open private proxy home parent: %w", err)
	}
	removeErr := removePrivateProxyEntryInRoot(parent, name)
	closeErr := parent.Close()
	return errors.Join(removeErr, closeErr)
}

func removePrivateProxyEntryInRoot(parent *os.Root, name string) error {
	info, err := parent.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %q: %w", name, err)
	}
	if !info.IsDir() {
		if err := parent.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %q: %w", name, err)
		}
		return nil
	}

	child, err := parent.OpenRoot(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open private proxy directory %q: %w", name, err)
	}
	contentsErr := removePrivateProxyRootContents(child)
	closeErr := child.Close()
	if err := errors.Join(contentsErr, closeErr); err != nil {
		return fmt.Errorf("remove private proxy directory %q: %w", name, err)
	}
	if err := parent.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove private proxy directory %q: %w", name, err)
	}
	return nil
}

func removePrivateProxyRootContents(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	entries, readErr := dir.ReadDir(-1)
	closeErr := dir.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return err
	}
	for _, entry := range entries {
		if err := removePrivateProxyEntryInRoot(root, entry.Name()); err != nil {
			return err
		}
	}
	return nil
}
