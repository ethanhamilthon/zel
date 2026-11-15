package store

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	id      TEXT PRIMARY KEY,
	project TEXT NOT NULL,
	title   TEXT NOT NULL DEFAULT '',
	first   TEXT NOT NULL DEFAULT '',
	updated INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_by_project ON sessions (project, updated);
CREATE TABLE IF NOT EXISTS items (
	session TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
	seq     INTEGER NOT NULL,
	data    TEXT NOT NULL,
	PRIMARY KEY (session, seq)
) WITHOUT ROWID;`

var migrations = []string{
	`CREATE TABLE preferences (id INTEGER PRIMARY KEY CHECK(id=1), data TEXT NOT NULL);`,
	`ALTER TABLE sessions ADD COLUMN provider TEXT NOT NULL DEFAULT '';
	 ALTER TABLE sessions ADD COLUMN model TEXT NOT NULL DEFAULT '';
	 ALTER TABLE sessions ADD COLUMN effort TEXT NOT NULL DEFAULT '';`,
	`CREATE TABLE tasks (
		session   TEXT NOT NULL,
		id        TEXT NOT NULL,
		command   TEXT NOT NULL,
		status    TEXT NOT NULL,
		exit_code INTEGER,
		started   INTEGER NOT NULL,
		ended     INTEGER,
		log       TEXT NOT NULL,
		PRIMARY KEY (session, id)
	);`,
	`ALTER TABLE items ADD COLUMN meta TEXT NOT NULL DEFAULT '';`,
	`CREATE TABLE kv (
		scope TEXT NOT NULL,
		key   TEXT NOT NULL,
		value TEXT NOT NULL,
		PRIMARY KEY (scope, key)
	) WITHOUT ROWID;`,
	`DROP TABLE kv;`, // state.* was removed in 0.6
}

func migrate(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version >= len(migrations) {
		return nil
	}
	if _, err := tx.Exec(schema); err != nil {
		return err
	}
	for _, m := range migrations[version:] {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, len(migrations))); err != nil {
		return err
	}
	return tx.Commit()
}
