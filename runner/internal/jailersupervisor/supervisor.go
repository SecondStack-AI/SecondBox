package jailersupervisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	InvocationArgument = "--secondbox-internal-jailer-supervisor"
	specEnvironment    = "SECONDBOX_INTERNAL_JAILER_SUPERVISOR_SPEC"
)

type commandSpec struct {
	Executable string   `json:"executable"`
	Arguments  []string `json:"arguments"`
}

// CommandEnvironment serializes one jailer invocation outside argv so process
// scans looking for Firecracker's --id do not mistake the supervisor for the
// Firecracker process it owns.
func CommandEnvironment(executable string, arguments []string) (string, error) {
	executable = strings.TrimSpace(executable)
	if executable == "" {
		return "", fmt.Errorf("SecondBox jailer supervisor executable is required")
	}
	encoded, err := json.Marshal(commandSpec{
		Executable: executable,
		Arguments:  append([]string(nil), arguments...),
	})
	if err != nil {
		return "", fmt.Errorf("encode SecondBox jailer supervisor command: %w", err)
	}
	return specEnvironment + "=" + string(encoded), nil
}

// RunInvocation handles the private re-exec mode used to keep each jailer's
// orphaned Firecracker child under one explicit subreaper.
func RunInvocation(arguments []string) (bool, error) {
	if len(arguments) == 0 || arguments[0] != InvocationArgument {
		return false, nil
	}
	if len(arguments) != 1 {
		return true, fmt.Errorf("SecondBox jailer supervisor accepts no command arguments")
	}

	var spec commandSpec
	if err := json.Unmarshal([]byte(os.Getenv(specEnvironment)), &spec); err != nil {
		return true, fmt.Errorf("decode SecondBox jailer supervisor command: %w", err)
	}
	spec.Executable = strings.TrimSpace(spec.Executable)
	if spec.Executable == "" {
		return true, fmt.Errorf("SecondBox jailer supervisor executable is required")
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return true, fmt.Errorf("configure SecondBox jailer child subreaper: %w", err)
	}

	command := exec.Command(spec.Executable, spec.Arguments...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.Env = environmentWithoutSupervisorSpec(os.Environ())
	if err := command.Start(); err != nil {
		return true, fmt.Errorf("start SecondBox jailer: %w", err)
	}
	jailerErr := command.Wait()
	reapErr := waitForDescendants()
	if jailerErr != nil {
		jailerErr = fmt.Errorf("SecondBox jailer exited: %w", jailerErr)
	}
	return true, errors.Join(jailerErr, reapErr)
}

func waitForDescendants() error {
	for {
		var status syscall.WaitStatus
		_, err := syscall.Wait4(-1, &status, 0, nil)
		switch {
		case err == nil:
			continue
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.ECHILD):
			return nil
		default:
			return fmt.Errorf("reap SecondBox jailer descendant: %w", err)
		}
	}
}

func environmentWithoutSupervisorSpec(environment []string) []string {
	prefix := specEnvironment + "="
	filtered := make([]string, 0, len(environment))
	for _, variable := range environment {
		if strings.HasPrefix(variable, prefix) {
			continue
		}
		filtered = append(filtered, variable)
	}
	return filtered
}
