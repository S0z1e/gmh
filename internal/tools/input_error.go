package tools

// InputError marks an argument rejection that workspace changes cannot repair.
// Preserve the original error and message for existing callers and displays.
type InputError struct{ Err error }

func (e *InputError) Error() string { return e.Err.Error() }
func (e *InputError) Unwrap() error { return e.Err }
