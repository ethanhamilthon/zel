package store

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// Open opens the database at path, creating it private to the user. Several zel
// processes share it: WAL lets readers run beside a writer, and writers wait for each other.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	// SQLite gives its -wal and -shm files the mode of the database file.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0600)
	if err != nil {
		return nil, err
	}
	file.Close()
	query := "_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", OmitHost: true, Path: path, RawQuery: query}).String())
	if err == nil {
		err = migrate(db)
	}
	if err != nil {
		if db != nil {
			db.Close()
		}
		return nil, fmt.Errorf("database %s: %w", path, err)
	}
	return &Store{DB: db}, nil
}

type Store struct{ *sql.DB }

var ErrNotFound = sql.ErrNoRows
