package store

import (
	"database/sql"
)

func (s *Store) Preferences() ([]byte, error) {
	var data []byte
	err := s.QueryRow(`SELECT data FROM preferences WHERE id = 1`).Scan(&data)
	if err == sql.ErrNoRows {
		err = nil
	}
	return data, err
}
func (s *Store) SetPreferences(data []byte) error {
	_, err := s.Exec(`INSERT INTO preferences(id,data) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data`, string(data))
	return err
}
