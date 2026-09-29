package ports

// InvalidFieldError names the one public request input that made a request
// invalid. The HTTP API reports every InvalidFieldError in an error tree as an
// invalid_request Problem detail.
//
// Field is the public JSON path in dotted camelCase (spec.resources.memoryBytes),
// a query parameter by name (limit), a header by its canonical name (If-Match),
// or body when the request body as a whole is malformed. Reason is a bounded
// client-facing predicate that completes the sentence "<Field> <Reason>"; it
// never embeds wrapped internal errors.
type InvalidFieldError struct {
	Field  string
	Reason string
}

func (err *InvalidFieldError) Error() string { return "SecondBox " + err.Field + " " + err.Reason }
func (err *InvalidFieldError) Unwrap() error { return ErrInvalidRequest }

// ResourceAlignmentError identifies a public size field refused before allocation.
func ResourceAlignmentError(field string) *InvalidFieldError {
	return &InvalidFieldError{Field: field, Reason: "must use whole MiB (multiples of 1048576 bytes)"}
}
