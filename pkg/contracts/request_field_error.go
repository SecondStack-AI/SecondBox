package contracts

// RequestFieldError reports a request value that a custom JSON decoder
// refused. Field is relative to the value whose decoder refused it and is
// empty when that value as a whole was refused; encoding/json does not tell a
// decoder where its value sits in the document, so the API locates the
// enclosing member path.
type RequestFieldError struct {
	Field  string
	Reason string
}

func (err *RequestFieldError) Error() string {
	if err.Field == "" {
		return "SecondBox request value " + err.Reason
	}
	return "SecondBox request field " + err.Field + " " + err.Reason
}
