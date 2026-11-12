package store

import (
	"context"
	"encoding/json"
	"time"
	"zel/internal/entities"
)

type Session struct{ ID, Project, Title, First, Provider, Model, Effort string }
type Item struct {
	Data json.RawMessage
	Meta *entities.Metadata
}

func (s *Store) Session(id, project string) (v Session, err error) {
	v.ID, v.Project = id, project
	err = s.QueryRow(`SELECT title, provider, model, effort FROM sessions WHERE id = ? AND project = ?`, id, project).Scan(&v.Title, &v.Provider, &v.Model, &v.Effort)
	return
}
func (s *Store) SelectModel(ctx context.Context, id, p, m, e string) error {
	_, err := s.ExecContext(ctx, `UPDATE sessions SET provider = ?, model = ?, effort = ? WHERE id = ?`, p, m, e, id)
	return err
}
func (s *Store) Items(id string) ([]Item, error) {
	rows, err := s.Query(`SELECT data, meta FROM items WHERE session = ? ORDER BY seq`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []Item
	for rows.Next() {
		var item Item
		var data []byte
		var meta string
		if err := rows.Scan(&data, &meta); err != nil {
			return nil, err
		}
		item.Data = json.RawMessage(data)
		if meta != "" {
			_ = json.Unmarshal([]byte(meta), &item.Meta)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
func (s *Store) SaveSession(v Session, history []json.RawMessage, metadata map[int]*entities.Metadata, saved int) error {
	tx, err := s.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT INTO sessions (id, project, title, first, updated, provider, model, effort) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
 ON CONFLICT (id) DO UPDATE SET title=excluded.title,first=excluded.first,updated=excluded.updated,provider=excluded.provider,model=excluded.model,effort=excluded.effort`, v.ID, v.Project, v.Title, v.First, time.Now().UnixNano(), v.Provider, v.Model, v.Effort); err != nil {
		return err
	}
	start := min(saved, len(history))
	if _, err = tx.Exec(`DELETE FROM items WHERE session = ? AND seq >= ?`, v.ID, start); err != nil {
		return err
	}
	for i := start; i < len(history); i++ {
		meta := ""
		if m := metadata[i]; m != nil {
			data, _ := json.Marshal(m)
			meta = string(data)
		}
		if _, err = tx.Exec(`INSERT INTO items (session, seq, data, meta) VALUES (?, ?, ?, ?)`, v.ID, i, string(history[i]), meta); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (db *Store) Sessions(project string) ([]entities.SessionSummary, error) {
	rows, err := db.Query(`SELECT id, CASE title WHEN '' THEN first ELSE title END, project, updated
		FROM sessions WHERE project = ? ORDER BY updated DESC, id DESC`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []entities.SessionSummary
	for rows.Next() {
		var s entities.SessionSummary
		var updated int64
		if err := rows.Scan(&s.ID, &s.Title, &s.Project, &updated); err != nil {
			return nil, err
		}
		s.Updated = time.Unix(0, updated)
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

func (s *Store) SessionProject(id string) (string, error) {
	var project string
	err := s.QueryRow(`SELECT project FROM sessions WHERE id = ?`, id).Scan(&project)
	return project, err
}

func (s *Store) RenameSession(id, title string) error {
	res, err := s.Exec(`UPDATE sessions SET title = ? WHERE id = ?`, title, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
