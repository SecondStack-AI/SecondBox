package secondboxclient

import (
	"encoding/json"
	"fmt"
	"reflect"
)

func (value Command) MarshalJSON() ([]byte, error) {
	if value.ShellCommand != nil && value.ArgvCommand == nil {
		return json.Marshal(value.ShellCommand)
	}
	if value.ArgvCommand != nil && value.ShellCommand == nil {
		return json.Marshal(value.ArgvCommand)
	}
	return nil, fmt.Errorf("SecondBox Command requires exactly one variant")
}

func (value *Command) UnmarshalJSON(data []byte) error {
	var discriminator struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(data, &discriminator); err != nil {
		return err
	}
	switch discriminator.Mode {
	case "shell":
		var decoded ShellCommand
		if err := json.Unmarshal(data, &decoded); err != nil {
			return err
		}
		*value = Command{ShellCommand: &decoded}
	case "argv":
		var decoded ArgvCommand
		if err := json.Unmarshal(data, &decoded); err != nil {
			return err
		}
		*value = Command{ArgvCommand: &decoded}
	default:
		return fmt.Errorf("SecondBox Command has unsupported discriminator %q", discriminator.Mode)
	}
	return nil
}

func (value ExecOutcome) MarshalJSON() ([]byte, error) {
	var selected any
	count := 0
	for _, variant := range []any{
		value.ExecExited, value.ExecSpawnFailed, value.ExecDeadlineExceeded,
		value.ExecCancelled, value.ExecOutputExhausted, value.ExecInfrastructureFailed,
	} {
		if !isNilPointer(variant) {
			selected = variant
			count++
		}
	}
	if count != 1 {
		return nil, fmt.Errorf("SecondBox ExecOutcome requires exactly one variant")
	}
	return json.Marshal(selected)
}

func (value *ExecOutcome) UnmarshalJSON(data []byte) error {
	var discriminator struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(data, &discriminator); err != nil {
		return err
	}
	switch discriminator.Kind {
	case "exited":
		var decoded ExecExited
		if err := json.Unmarshal(data, &decoded); err != nil {
			return err
		}
		*value = ExecOutcome{ExecExited: &decoded}
	case "spawn_failed":
		var decoded ExecSpawnFailed
		if err := json.Unmarshal(data, &decoded); err != nil {
			return err
		}
		*value = ExecOutcome{ExecSpawnFailed: &decoded}
	case "deadline_exceeded":
		var decoded ExecDeadlineExceeded
		if err := json.Unmarshal(data, &decoded); err != nil {
			return err
		}
		*value = ExecOutcome{ExecDeadlineExceeded: &decoded}
	case "cancelled":
		var decoded ExecCancelled
		if err := json.Unmarshal(data, &decoded); err != nil {
			return err
		}
		*value = ExecOutcome{ExecCancelled: &decoded}
	case "output_exhausted":
		var decoded ExecOutputExhausted
		if err := json.Unmarshal(data, &decoded); err != nil {
			return err
		}
		*value = ExecOutcome{ExecOutputExhausted: &decoded}
	case "infrastructure_failed":
		var decoded ExecInfrastructureFailed
		if err := json.Unmarshal(data, &decoded); err != nil {
			return err
		}
		*value = ExecOutcome{ExecInfrastructureFailed: &decoded}
	default:
		return fmt.Errorf("SecondBox ExecOutcome has unsupported discriminator %q", discriminator.Kind)
	}
	return nil
}

func isNilPointer(value any) bool {
	return value == nil || reflect.ValueOf(value).IsNil()
}

func (value ExecStreamFrame) MarshalJSON() ([]byte, error) {
	var selected any
	count := 0
	for _, variant := range []any{
		value.StreamInputFrame, value.StreamOutputFrame, value.StreamCreditFrame,
		value.StreamSignalFrame, value.StreamCancelFrame, value.StreamOutcomeFrame,
	} {
		if !isNilPointer(variant) {
			selected = variant
			count++
		}
	}
	if count != 1 {
		return nil, fmt.Errorf("SecondBox ExecStreamFrame requires exactly one variant")
	}
	return json.Marshal(selected)
}

func (value *ExecStreamFrame) UnmarshalJSON(data []byte) error {
	return unmarshalStreamFrame(data, func(kind string, decoded any) {
		switch kind {
		case "stdin":
			value.StreamInputFrame = decoded.(*StreamInputFrame)
		case "output":
			value.StreamOutputFrame = decoded.(*StreamOutputFrame)
		case "credit":
			value.StreamCreditFrame = decoded.(*StreamCreditFrame)
		case "signal":
			value.StreamSignalFrame = decoded.(*StreamSignalFrame)
		case "cancel":
			value.StreamCancelFrame = decoded.(*StreamCancelFrame)
		case "outcome":
			value.StreamOutcomeFrame = decoded.(*StreamOutcomeFrame)
		}
	})
}

func (value TerminalFrame) MarshalJSON() ([]byte, error) {
	var selected any
	count := 0
	for _, variant := range []any{
		value.TerminalAttachedFrame, value.TerminalInputFrame, value.TerminalOutputFrame, value.TerminalResizeFrame,
		value.StreamCreditFrame, value.StreamCancelFrame, value.StreamOutcomeFrame,
	} {
		if !isNilPointer(variant) {
			selected = variant
			count++
		}
	}
	if count != 1 {
		return nil, fmt.Errorf("SecondBox TerminalFrame requires exactly one variant")
	}
	return json.Marshal(selected)
}

func (value *TerminalFrame) UnmarshalJSON(data []byte) error {
	var discriminator struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &discriminator); err != nil {
		return err
	}
	var decoded any
	switch discriminator.Type {
	case "terminal_attached":
		decoded = &TerminalAttachedFrame{}
	case "terminal_input":
		decoded = &TerminalInputFrame{}
	case "terminal_output":
		decoded = &TerminalOutputFrame{}
	case "resize":
		decoded = &TerminalResizeFrame{}
	case "credit":
		decoded = &StreamCreditFrame{}
	case "cancel":
		decoded = &StreamCancelFrame{}
	case "outcome":
		decoded = &StreamOutcomeFrame{}
	default:
		return fmt.Errorf("SecondBox TerminalFrame has unsupported discriminator %q", discriminator.Type)
	}
	if err := json.Unmarshal(data, decoded); err != nil {
		return err
	}
	switch typed := decoded.(type) {
	case *TerminalAttachedFrame:
		value.TerminalAttachedFrame = typed
	case *TerminalInputFrame:
		value.TerminalInputFrame = typed
	case *TerminalOutputFrame:
		value.TerminalOutputFrame = typed
	case *TerminalResizeFrame:
		value.TerminalResizeFrame = typed
	case *StreamCreditFrame:
		value.StreamCreditFrame = typed
	case *StreamCancelFrame:
		value.StreamCancelFrame = typed
	case *StreamOutcomeFrame:
		value.StreamOutcomeFrame = typed
	}
	return nil
}

func unmarshalStreamFrame(data []byte, set func(string, any)) error {
	var discriminator struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &discriminator); err != nil {
		return err
	}
	var decoded any
	switch discriminator.Type {
	case "stdin":
		decoded = &StreamInputFrame{}
	case "output":
		decoded = &StreamOutputFrame{}
	case "credit":
		decoded = &StreamCreditFrame{}
	case "signal":
		decoded = &StreamSignalFrame{}
	case "cancel":
		decoded = &StreamCancelFrame{}
	case "outcome":
		decoded = &StreamOutcomeFrame{}
	default:
		return fmt.Errorf("SecondBox stream frame has unsupported discriminator %q", discriminator.Type)
	}
	if err := json.Unmarshal(data, decoded); err != nil {
		return err
	}
	set(discriminator.Type, decoded)
	return nil
}
