package store

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Task is one background task of a session, from tasks.jsonl.
type Task struct {
	ID, Name, Command, Status, Log string
	ExitCode                       *int
	Started, Ended                 time.Time
}

type taskLine struct {
	T      string    `json:"t"` // start or end
	ID     string    `json:"id"`
	Name   string    `json:"name,omitempty"`
	Cmd    string    `json:"cmd,omitempty"`
	Status string    `json:"status,omitempty"`
	Code   *int      `json:"code,omitempty"`
	At     time.Time `json:"at"`
}

func (s *Store) tasksFile(project, session string) string {
	return filepath.Join(s.Dir(project, session), "tasks.jsonl")
}

// TaskLog is the output file of a task.
func (s *Store) TaskLog(project, session, id string) string {
	return filepath.Join(s.Dir(project, session), "tasks", id+".log")
}

// Tasks lists the tasks of a session in start order.
func (s *Store) Tasks(project, session string) ([]Task, error) {
	file, err := os.Open(s.tasksFile(project, session))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var list []Task
	index := map[string]int{}
	scan := bufio.NewScanner(file)
	scan.Buffer(make([]byte, 64<<10), 4<<20)
	for scan.Scan() {
		var l taskLine
		if json.Unmarshal(scan.Bytes(), &l) != nil {
			continue
		}
		switch l.T {
		case "start":
			index[l.ID] = len(list)
			list = append(list, Task{ID: l.ID, Name: l.Name, Command: l.Cmd, Status: "running", Started: l.At,
				Log: s.TaskLog(project, session, l.ID)})
		case "end":
			if i, ok := index[l.ID]; ok {
				list[i].Status, list[i].ExitCode, list[i].Ended = l.Status, l.Code, l.At
			}
		}
	}
	return list, scan.Err()
}

// CreateTask records a task that starts now.
func (s *Store) CreateTask(project, session, id, name, command string, started time.Time) error {
	if err := os.MkdirAll(filepath.Join(s.Dir(project, session), "tasks"), 0700); err != nil {
		return err
	}
	return s.appendTask(project, session, taskLine{T: "start", ID: id, Name: name, Cmd: command, At: started})
}

// FinishTask records how a task ended: done, failed, stopped, timeout or lost.
func (s *Store) FinishTask(project, session, id, status string, exit *int) error {
	return s.appendTask(project, session, taskLine{T: "end", ID: id, Status: status, Code: exit, At: time.Now().UTC()})
}

// LoseTasks marks the tasks of an earlier run that never ended as lost.
func (s *Store) LoseTasks(project, session string) error {
	list, err := s.Tasks(project, session)
	for _, t := range list {
		if err == nil && t.Status == "running" {
			err = s.FinishTask(project, session, t.ID, "lost", nil)
		}
	}
	return err
}

func (s *Store) appendTask(project, session string, l taskLine) error {
	data, err := json.Marshal(l)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir(project, session), 0700); err != nil {
		return err
	}
	return appendFile(s.tasksFile(project, session), append(data, '\n'), false)
}
