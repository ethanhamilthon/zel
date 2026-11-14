package core

type providerError struct {
	text  string
	cause error
}

func (e *providerError) Error() string { return e.text }
func (e *providerError) Unwrap() error { return e.cause }

func (s *session) safeError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := err.(*providerError); ok {
		return err
	}
	text := err.Error()
	return &providerError{text: Redact(s.Settings(), text), cause: err}
}
