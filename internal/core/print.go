package core

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// Like answered choices, these records are anchored to model history without
// becoming part of any model request or changing its cache.
type printLine struct {
	Seq  int
	Text string
	At   time.Time
}

func (s *session) printsPath() string { return filepath.Join(s.sessionDir(), "prints.jsonl") }

func (s *session) loadPrints() error {
	file, err := os.Open(s.printsPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	for {
		data, err := reader.ReadBytes('\n')
		var line printLine
		if json.Unmarshal(data, &line) == nil {
			s.printLines = append(s.printLines, line)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	s.printPublished = len(s.printLines)
	return nil
}

// print persists a user-visible message without waking the model.
func (s *session) print(text string) error {
	if err := s.storePrint(text); err != nil {
		return err
	}
	if s.printWake != nil {
		s.printWake.notify()
	}
	return nil
}

// printLine publishes command output. The caller holds c.mu.
func (c *Controller) printLine(text string) {
	if err := c.a.session().storePrint(text); err != nil {
		c.emit(Event{Kind: "command_error", Text: fmt.Sprintf("could not save command output: %v", err)})
		return
	}
	c.publishPrintsLocked()
}

func (s *session) storePrint(text string) error {
	s.histMu.RLock()
	stored := s.stored
	s.histMu.RUnlock()
	if !stored { // printing before the first turn still creates resumable history
		if err := s.Save(); err != nil {
			return err
		}
	}
	s.histMu.Lock()
	defer s.histMu.Unlock()
	line := printLine{Seq: len(s.History), Text: text, At: time.Now()}
	file, err := os.OpenFile(s.printsPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	err = json.NewEncoder(file).Encode(line)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	s.printLines = append(s.printLines, line)
	return nil
}

func (l printLine) record(id string, index int) Record {
	return Record{Kind: "print", Text: l.Text, ID: fmt.Sprintf("print-%s-%d", id, index), Meta: &Metadata{At: l.At}}
}

func (s *session) takePrints() (int, []printLine) {
	s.histMu.Lock()
	defer s.histMu.Unlock()
	start := s.printPublished
	s.printPublished = len(s.printLines)
	return start, slices.Clone(s.printLines[start:])
}

func (c *Controller) publishPrintsLocked() {
	if c.switching || c.shut {
		return
	}
	s := c.a.session()
	start, lines := s.takePrints()
	for i, line := range lines {
		r := line.record(s.id, start+i)
		c.emit(Event{Kind: "agent", Agent: &TurnEvent{Kind: r.Kind, Text: r.Text, ID: r.ID, Meta: r.Meta, Final: true}})
	}
}
