package store

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Choice is an answered zel choice question, from choices.jsonl. Seq is the
// history length when it was answered: the chat shows it before item Seq.
type Choice struct {
	Seq    int       `json:"seq"`
	Title  string    `json:"title"`
	Answer string    `json:"answer"`
	At     time.Time `json:"at"`
}

func (s *Store) choicesFile(project, session string) string {
	return filepath.Join(s.Dir(project, session), "choices.jsonl")
}

// AppendChoice records an answered question.
func (s *Store) AppendChoice(project, session string, c Choice) error {
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir(project, session), 0700); err != nil {
		return err
	}
	return appendFile(s.choicesFile(project, session), append(data, '\n'), true)
}

// Choices lists the answered questions of a session in answer order; torn lines are skipped.
func (s *Store) Choices(project, session string) ([]Choice, error) {
	file, err := os.Open(s.choicesFile(project, session))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var list []Choice
	scan := bufio.NewScanner(file)
	scan.Buffer(make([]byte, 64<<10), 4<<20)
	for scan.Scan() {
		var c Choice
		if json.Unmarshal(scan.Bytes(), &c) == nil {
			list = append(list, c)
		}
	}
	return list, scan.Err()
}
