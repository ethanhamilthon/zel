package store

import (
	"database/sql"
	"time"
	"zel/internal/entities"
)

func (db *Store) Tasks(session string) ([]entities.Task, error) {
	rows, err := db.Query(`SELECT id, command, status, log, exit_code, started, ended FROM tasks WHERE session = ? ORDER BY started`, session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []entities.Task
	for rows.Next() {
		var task entities.Task
		var code, ended sql.NullInt64
		var started int64
		if err := rows.Scan(&task.ID, &task.Command, &task.Status, &task.Log, &code, &started, &ended); err != nil {
			return nil, err
		}
		if code.Valid {
			c := int(code.Int64)
			task.ExitCode = &c
		}
		task.Started = time.Unix(0, started)
		if ended.Valid {
			task.Ended = time.Unix(0, ended.Int64)
		}
		list = append(list, task)
	}
	return list, rows.Err()
}

func (s *Store) LoseTasks(session string) error {
	_, err := s.Exec(`UPDATE tasks SET status = 'lost', ended = ? WHERE session = ? AND status = 'running'`, time.Now().UnixNano(), session)
	return err
}
func (s *Store) TaskCount(session string, n *int) error {
	return s.QueryRow(`SELECT COUNT(*) FROM tasks WHERE session = ?`, session).Scan(n)
}
func (s *Store) CreateTask(session, id, command string, started time.Time, log string) error {
	_, err := s.Exec(`INSERT INTO tasks (session, id, command, status, started, log) VALUES (?, ?, ?, 'running', ?, ?)`, session, id, command, started.UnixNano(), log)
	return err
}
func (s *Store) FinishTask(session, id, status string, exit *int) error {
	_, err := s.Exec(`UPDATE tasks SET status = ?, exit_code = ?, ended = ? WHERE session = ? AND id = ?`, status, exit, time.Now().UnixNano(), session, id)
	return err
}
