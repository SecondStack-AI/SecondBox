package cliui

import (
	"context"
	"errors"
	"fmt"
	"io"

	"charm.land/huh/v2"
	"github.com/charmbracelet/x/ansi"
)

type FormHandles struct {
	Input      io.Reader
	Output     io.Writer
	Width      int
	Accessible bool
	Dark       bool
}

// Form is the command-facing boundary. Tests and non-interactive planners can
// inject an implementation without importing Charm.
type Form interface {
	Run(context.Context, FormHandles) error
}

type FieldKind string

const (
	FieldText    FieldKind = "text"
	FieldSecret  FieldKind = "secret"
	FieldSelect  FieldKind = "select"
	FieldConfirm FieldKind = "confirm"
)

type Option struct{ Label, Value string }

type FieldSpec struct {
	Kind               FieldKind
	Title              string
	Description        string
	StringValue        *string
	BoolValue          *bool
	Options            []Option
	ValidateString     func(string) error
	RequireAffirmative bool
}

type GroupSpec struct {
	Title  string
	Fields []FieldSpec
}

type HuhForm struct{ Groups []GroupSpec }

func (spec HuhForm) Run(ctx context.Context, handles FormHandles) error {
	if handles.Input == nil || handles.Output == nil {
		return errors.New("SecondBox CLI form requires explicit input and output")
	}
	groups := make([]*huh.Group, 0, len(spec.Groups))
	accessibleGroups := make([][]huh.Field, 0, len(spec.Groups))
	for _, groupSpec := range spec.Groups {
		fields := make([]huh.Field, 0, len(groupSpec.Fields))
		for _, fieldSpec := range groupSpec.Fields {
			field, err := buildField(fieldSpec)
			if err != nil {
				return err
			}
			fields = append(fields, field)
		}
		group := huh.NewGroup(fields...)
		if groupSpec.Title != "" {
			group = group.Title(groupSpec.Title)
		}
		groups = append(groups, group)
		accessibleGroups = append(accessibleGroups, fields)
	}
	if handles.Accessible {
		accessibleOutput := ansiStrippingWriter{target: handles.Output}
		// Huh creates a new buffered Scanner for each field. Bound each read so
		// one field cannot consume pasted answers intended for later fields or
		// later form groups.
		accessibleInput := newEndTrackingReader(boundedAccessibleReader(handles.Input))
		for groupIndex, fields := range accessibleGroups {
			if title := spec.Groups[groupIndex].Title; title != "" {
				if _, err := fmt.Fprintln(accessibleOutput, Sanitize(title)); err != nil {
					return err
				}
			}
			for fieldIndex, field := range fields {
				if accessibleInput.state().ended {
					return errors.New("SecondBox CLI accessible form: input ended before every field was answered")
				}
				consumedBefore := accessibleInput.state().consumed
				if err := runAccessibleField(field, accessibleOutput, accessibleInput); err != nil {
					return fmt.Errorf("SecondBox CLI accessible form: %w", err)
				}
				// A field that read nothing before end of input must not stand as
				// answered: Huh treats the empty read as accepting whatever the
				// field held. An answer without a trailing newline is fine.
				if state := accessibleInput.state(); state.ended && state.consumed == consumedBefore {
					return errors.New("SecondBox CLI accessible form: input ended before every field was answered")
				}
				spec := spec.Groups[groupIndex].Fields[fieldIndex]
				if spec.ValidateString != nil && spec.StringValue != nil {
					if err := spec.ValidateString(*spec.StringValue); err != nil {
						return fmt.Errorf("SecondBox CLI accessible form: %w", err)
					}
				}
				if spec.RequireAffirmative && (spec.BoolValue == nil || !*spec.BoolValue) {
					return errors.New("SecondBox CLI accessible form: an affirmative choice is required")
				}
			}
		}
		return nil
	}
	width := handles.Width
	if width <= 0 {
		width = 80
	}
	form := huh.NewForm(groups...).
		WithInput(handles.Input).
		WithOutput(handles.Output).
		WithWidth(width).
		WithAccessible(handles.Accessible)
	form = form.WithTheme(huh.ThemeFunc(func(bool) *huh.Styles {
		return huh.ThemeBase16(handles.Dark)
	}))
	if err := form.RunWithContext(ctx); err != nil {
		return fmt.Errorf("SecondBox CLI form: %w", err)
	}
	return nil
}

type ansiStrippingWriter struct{ target io.Writer }

// endTrackingReader remembers that the scripted or piped answers ran out and
// how many bytes each field consumed, so the form can refuse a field that met
// end of input before reading an answer instead of letting Huh accept the
// field's current value. The descriptor-carrying variant keeps Fd visible,
// which Huh needs to read a real terminal (password fields refuse otherwise).
type endTrackingState struct {
	ended    bool
	consumed int
}

type endTrackingInput interface {
	io.Reader
	state() endTrackingState
}

type endTrackingReader struct {
	target io.Reader
	track  endTrackingState
}

type endTrackingFileDescriptorReader struct {
	*endTrackingReader
	descriptor fileDescriptorReader
}

func newEndTrackingReader(target io.Reader) endTrackingInput {
	reader := &endTrackingReader{target: target}
	if descriptor, ok := target.(fileDescriptorReader); ok {
		return endTrackingFileDescriptorReader{endTrackingReader: reader, descriptor: descriptor}
	}
	return reader
}

func (reader *endTrackingReader) Read(content []byte) (int, error) {
	count, err := reader.target.Read(content)
	reader.track.consumed += count
	if errors.Is(err, io.EOF) {
		reader.track.ended = true
	}
	return count, err
}

func (reader *endTrackingReader) state() endTrackingState { return reader.track }

func (reader endTrackingFileDescriptorReader) Fd() uintptr { return reader.descriptor.Fd() }

// runAccessibleField converts a field's panic on exhausted input, which Huh's
// accessible select raises as an index error, into an ordinary error so an
// installer driven by piped answers fails with a message rather than a trace.
func runAccessibleField(field huh.Field, output io.Writer, input io.Reader) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("input ended before the field was answered: %v", recovered)
		}
	}()
	return field.RunAccessible(output, input)
}

type singleByteReader struct{ target io.Reader }

type fileDescriptorReader interface {
	io.Reader
	Fd() uintptr
}

type singleByteFileDescriptorReader struct{ target fileDescriptorReader }

func boundedAccessibleReader(target io.Reader) io.Reader {
	if descriptor, ok := target.(fileDescriptorReader); ok {
		return singleByteFileDescriptorReader{target: descriptor}
	}
	return singleByteReader{target: target}
}

func (reader singleByteReader) Read(content []byte) (int, error) {
	if len(content) > 1 {
		content = content[:1]
	}
	return reader.target.Read(content)
}

func (reader singleByteFileDescriptorReader) Read(content []byte) (int, error) {
	if len(content) > 1 {
		content = content[:1]
	}
	return reader.target.Read(content)
}

func (reader singleByteFileDescriptorReader) Fd() uintptr { return reader.target.Fd() }

func (writer ansiStrippingWriter) Write(content []byte) (int, error) {
	_, err := io.WriteString(writer.target, ansi.Strip(string(content)))
	if err != nil {
		return 0, err
	}
	return len(content), nil
}

func buildField(spec FieldSpec) (huh.Field, error) {
	switch spec.Kind {
	case FieldText, FieldSecret:
		if spec.StringValue == nil {
			return nil, errors.New("SecondBox CLI form text field requires a value target")
		}
		field := huh.NewInput().Title(Sanitize(spec.Title)).Description(Sanitize(spec.Description)).Value(spec.StringValue)
		if spec.Kind == FieldSecret {
			field = field.EchoMode(huh.EchoModePassword)
		}
		if spec.ValidateString != nil {
			field = field.Validate(spec.ValidateString)
		}
		return field, nil
	case FieldSelect:
		if spec.StringValue == nil || len(spec.Options) == 0 {
			return nil, errors.New("SecondBox CLI form select field requires a value target and options")
		}
		options := make([]huh.Option[string], 0, len(spec.Options))
		for _, option := range spec.Options {
			options = append(options, huh.NewOption(Sanitize(option.Label), option.Value))
		}
		field := huh.NewSelect[string]().Title(Sanitize(spec.Title)).Description(Sanitize(spec.Description)).Options(options...).Value(spec.StringValue)
		if spec.ValidateString != nil {
			field = field.Validate(spec.ValidateString)
		}
		return field, nil
	case FieldConfirm:
		if spec.BoolValue == nil {
			return nil, errors.New("SecondBox CLI form confirmation requires a value target")
		}
		field := huh.NewConfirm().Title(Sanitize(spec.Title)).Description(Sanitize(spec.Description)).Affirmative("Yes").Negative("No").Value(spec.BoolValue)
		if spec.RequireAffirmative {
			field = field.Validate(func(value bool) error {
				if !value {
					return errors.New("an affirmative choice is required")
				}
				return nil
			})
		}
		return field, nil
	default:
		return nil, fmt.Errorf("SecondBox CLI form field kind %q is unsupported", spec.Kind)
	}
}
