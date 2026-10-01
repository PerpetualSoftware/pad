package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"modernc.org/sqlite"
)

// restoreSQLite publishes one self-contained database, never a partially copied
// main/WAL tuple. The server must be stopped throughout this operation.
func restoreSQLite(inputPath, dstPath string) error {
	mode := os.FileMode(0o600)
	var dstInfo os.FileInfo
	if info, err := os.Stat(dstPath); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("database is not a regular file: %s", dstPath)
		}
		mode = info.Mode().Perm()
		dstInfo = info
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat database: %w", err)
	}
	// Keep configured symlinks intact, including links whose database was lost.
	var err error
	dstPath, err = resolveSQLiteRestorePath(dstPath)
	if err != nil {
		return fmt.Errorf("resolve database path: %w", err)
	}

	stage, err := os.MkdirTemp(filepath.Dir(dstPath), ".pad-restore-*")
	if err != nil {
		return fmt.Errorf("stage restore: %w", err)
	}
	defer os.RemoveAll(stage)
	staged := filepath.Join(stage, "backup.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := stageSQLiteRestoreFile(inputPath+suffix, staged+suffix, suffix != ""); err != nil {
			return fmt.Errorf("stage backup%s: %w", suffix, err)
		}
	}

	// Read the private copy through SQLite, incorporating committed source WAL
	// pages without modifying the original backup or publishing its SHM cache.
	if err := checkSQLiteRestoreDatabase(staged); err != nil {
		return fmt.Errorf("check backup database: %w", err)
	}
	publish := filepath.Join(stage, "restored.db")
	if err := normalizeSQLiteRestore(staged, publish); err != nil {
		return fmt.Errorf("prepare restored database: %w", err)
	}
	f, err := os.OpenFile(publish, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open staged database: %w", err)
	}
	err = errors.Join(preserveSQLiteRestoreOwnership(f, dstInfo), f.Chmod(mode), f.Sync(), f.Close())
	if err != nil {
		return fmt.Errorf("sync staged database: %w", err)
	}

	// A live WAL is durable data, not disposable cache. Checkpoint it before
	// removing sidecars so any later preparation/rename failure leaves the old
	// committed contents readable from the main file. Never discard a WAL that
	// SQLite cannot checkpoint (including a corrupt database or a held lock).
	hasSidecars := false
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		info, err := os.Stat(dstPath + suffix)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("stat database%s: %w", suffix, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("database%s is not a regular file", suffix)
		}
		hasSidecars = true
	}
	if hasSidecars {
		// Even closing a failed live SQLite connection can checkpoint its WAL.
		// Validate a private copy first so a corrupt tuple is left untouched.
		oldCopy := filepath.Join(stage, "current.db")
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			if err := stageSQLiteRestoreFile(dstPath+suffix, oldCopy+suffix, suffix != ""); err != nil {
				return fmt.Errorf("stage current database%s: %w", suffix, err)
			}
		}
		if err := checkSQLiteRestoreDatabase(oldCopy); err != nil {
			return fmt.Errorf("check current database before checkpoint: %w", err)
		}
		dsn, err := sqliteFileURI(dstPath)
		if err != nil {
			return err
		}
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			return fmt.Errorf("open database for checkpoint: %w", err)
		}
		var journalMode string
		err = db.QueryRow("PRAGMA journal_mode=DELETE").Scan(&journalMode)
		err = errors.Join(err, db.Close())
		if err != nil {
			return fmt.Errorf("checkpoint database before restore: %w", err)
		}
		if journalMode != "delete" {
			return fmt.Errorf("checkpoint database before restore: journal mode remains %s", journalMode)
		}
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if err := os.Remove(dstPath + suffix); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove checkpointed %s: %w", suffix, err)
		}
	}
	if err := os.Rename(publish, dstPath); err != nil {
		return fmt.Errorf("publish restored database: %w", err)
	}
	return nil
}

func resolveSQLiteRestorePath(path string) (string, error) {
	for range 40 {
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil || !os.IsNotExist(err) {
			return resolved, err
		}
		info, err := os.Lstat(path)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return "", err
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(path), target)
			}
			path = target
			continue
		}
		if err != nil && !os.IsNotExist(err) {
			return "", err
		}
		parent, err := filepath.EvalSymlinks(filepath.Dir(path))
		return filepath.Join(parent, filepath.Base(path)), err
	}
	return "", errors.New("too many destination symlinks")
}

// SQLite's backup API preserves hidden row IDs (unlike VACUUM INTO), including
// those used by external-content FTS indexes. Its destination is a fresh staged
// file, so live WAL mode, page size and corruption cannot impede the copy.
func normalizeSQLiteRestore(srcPath, dstPath string) (retErr error) {
	dsn, err := sqliteFileURI(srcPath)
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, db.Close()) }()
	conn, err := db.Conn(context.Background())
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, conn.Close()) }()
	dstURI, err := sqliteFileURI(dstPath)
	if err != nil {
		return err
	}
	return conn.Raw(func(raw any) error {
		backup, err := raw.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		}).NewBackup(dstURI)
		if err != nil {
			return err
		}
		more, stepErr := backup.Step(-1)
		if more && stepErr == nil {
			stepErr = errors.New("SQLite backup did not finish")
		}
		// Finalize even on failure to roll back the private destination, and
		// close it explicitly: Backup.Finish discards its Close error.
		dst, finishErr := backup.Commit()
		if dst != nil {
			finishErr = errors.Join(finishErr, dst.Close())
		}
		return errors.Join(stepErr, finishErr)
	})
}

func checkSQLiteRestoreDatabase(path string) error {
	dsn, err := sqliteFileURI(path)
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	var result string
	err = db.QueryRow("PRAGMA quick_check").Scan(&result)
	err = errors.Join(err, db.Close())
	if err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("SQLite quick_check: %s", result)
	}
	return nil
}

// Escape filesystem names rather than letting SQLite interpret '?' or '#' as
// connection options. Windows drive paths need a leading slash in file URIs.
func sqliteFileURI(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	path = filepath.ToSlash(abs)
	if filepath.VolumeName(abs) != "" && !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return (&url.URL{Scheme: "file", Path: path}).String(), nil
}

func stageSQLiteRestoreFile(srcPath, dstPath string, optional bool) (retErr error) {
	info, err := os.Stat(srcPath)
	if optional && os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || (!optional && info.Size() == 0) {
		return fmt.Errorf("not a non-empty regular database file: %s", srcPath)
	}
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, src.Close()) }()
	// Recheck the open file before copying, rather than relying on path stat.
	info, err = src.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular file: %s", srcPath)
	}
	dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, dst.Close()) }()
	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	return dst.Sync()
}
