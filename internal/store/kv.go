package store

import (
	"database/sql"
	"errors"
)

// The kv table backs state.get/set/list of the Lua host API. A scope is an
// opaque string such as "session:<id>" or "project:<root>"; values are JSON text.

func (s *Store) KVGet(scope, key string) (string, bool, error) {
	var value string
	err := s.QueryRow(`SELECT value FROM kv WHERE scope = ? AND key = ?`, scope, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return value, err == nil, err
}

func (s *Store) KVSet(scope, key, value string) error {
	_, err := s.Exec(`INSERT INTO kv (scope, key, value) VALUES (?, ?, ?) ON CONFLICT (scope, key) DO UPDATE SET value = excluded.value`, scope, key, value)
	return err
}

func (s *Store) KVDelete(scope, key string) error {
	_, err := s.Exec(`DELETE FROM kv WHERE scope = ? AND key = ?`, scope, key)
	return err
}

// KVList returns the keys with the prefix, sorted.
func (s *Store) KVList(scope, prefix string) ([]string, error) {
	rows, err := s.Query(`SELECT key FROM kv WHERE scope = ? AND substr(key, 1, ?) = ? ORDER BY key`, scope, len([]rune(prefix)), prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := []string{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

// KVSize is the number of bytes of keys and values in the scope.
func (s *Store) KVSize(scope string) (int, error) {
	var n int
	err := s.QueryRow(`SELECT COALESCE(SUM(length(CAST(key AS BLOB)) + length(CAST(value AS BLOB))), 0) FROM kv WHERE scope = ?`, scope).Scan(&n)
	return n, err
}

// KVDeleteScope removes every key of the scope.
func (s *Store) KVDeleteScope(scope string) error {
	_, err := s.Exec(`DELETE FROM kv WHERE scope = ?`, scope)
	return err
}
