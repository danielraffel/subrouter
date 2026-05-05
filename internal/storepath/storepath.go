package storepath

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const stateDirEnv = "SUBROUTER_STATE_DIR"

func StateDir() string {
	if dir := strings.TrimSpace(os.Getenv(stateDirEnv)); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".subrouter"
	}
	return filepath.Join(home, ".subrouter")
}

func CodexDir() string {
	dir := filepath.Join(StateDir(), "codex")
	_ = MigrateLegacyCodexDir(dir)
	return dir
}

func LegacyCodexDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".codex-accounts"
	}
	return filepath.Join(home, ".codex-accounts")
}

func MigrateLegacyCodexDir(target string) error {
	return MigrateCodexDir(target, LegacyCodexDir())
}

func MigrateCodexDir(target, legacy string) error {
	target = filepath.Clean(target)
	legacy = filepath.Clean(legacy)
	if target == legacy {
		return nil
	}
	info, err := os.Stat(legacy)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return nil
	}
	nonEmpty, err := dirNonEmpty(target)
	if err != nil {
		return err
	}
	if nonEmpty {
		return nil
	}
	if exists, err := pathExists(target); err != nil {
		return err
	} else if exists {
		return copyDirContents(legacy, target)
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(target), "."+filepath.Base(target)+".migrate-*")
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(tmp)
		}
	}()
	if err := copyDirContents(legacy, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		nonEmpty, checkErr := dirNonEmpty(target)
		if checkErr == nil && nonEmpty {
			return nil
		}
		return err
	}
	cleanup = false
	return nil
}

func pathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func dirNonEmpty(dir string) (bool, error) {
	nonEmpty := false
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == dir {
			return nil
		}
		if shouldSkipLegacyEntry(entry.Name()) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		nonEmpty = true
		return filepath.SkipAll
	})
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return nonEmpty, nil
}

func copyDirContents(src, dst string) error {
	return filepath.WalkDir(src, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == src {
			return nil
		}
		if shouldSkipLegacyEntry(entry.Name()) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			mode := info.Mode().Perm()
			if mode == 0 {
				mode = 0o700
			}
			return os.MkdirAll(target, mode)
		}
		return copyFile(path, target, info.Mode().Perm())
	})
}

func shouldSkipLegacyEntry(name string) bool {
	return strings.HasPrefix(name, "._") || strings.HasSuffix(name, ".lock") || (strings.HasPrefix(name, ".") && strings.Contains(name, ".tmp-"))
}

func copyFile(src, dst string, mode os.FileMode) error {
	if mode == 0 {
		mode = 0o600
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		_ = out.Close()
		if cleanup {
			_ = os.Remove(dst)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	cleanup = false
	return nil
}
