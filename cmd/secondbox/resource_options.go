package main

import (
	"errors"
	"flag"
	"fmt"
	"math"
	"math/bits"
	"strconv"
	"strings"

	sb "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

// SandboxSizePresets are CLI conveniences; the server owns Profile ceilings.
var SandboxSizePresets = map[string]sb.SandboxResources{
	"small":  {VCPUCount: 1, MemoryBytes: 1 << 30, WorkspaceBytes: 4 << 30},
	"medium": {VCPUCount: 2, MemoryBytes: 4 << 30, WorkspaceBytes: 16 << 30},
	"large":  {VCPUCount: 4, MemoryBytes: 8 << 30, WorkspaceBytes: 50 << 30},
}

type resourceOptions struct{ size, cpus, memory, disk string }

func (options *resourceOptions) register(flags *flag.FlagSet) {
	flags.StringVar(&options.size, "size", "", "small, medium, or large; explicit axes override the preset")
	flags.StringVar(&options.cpus, "cpus", "", "whole vCPU count")
	flags.StringVar(&options.memory, "memory", "", "memory in bytes or KiB/MiB/GiB (k/m/g)")
	flags.StringVar(&options.disk, "disk", "", "Workspace capacity in bytes or KiB/MiB/GiB (k/m/g); rounds up to a power of two")
}

func (options resourceOptions) resolve(flags *flag.FlagSet) (*sb.SandboxResourceRequest, error) {
	present := map[string]bool{}
	flags.Visit(func(value *flag.Flag) { present[value.Name] = true })
	if !present["size"] && !present["cpus"] && !present["memory"] && !present["disk"] {
		return nil, nil
	}
	request := &sb.SandboxResourceRequest{}
	if present["size"] {
		preset, ok := SandboxSizePresets[options.size]
		if !ok {
			return nil, fmt.Errorf("SecondBox CLI %s --size must be small, medium, or large", flags.Name())
		}
		request.VCPUCount, request.MemoryBytes, request.WorkspaceBytes = &preset.VCPUCount, &preset.MemoryBytes, &preset.WorkspaceBytes
	}
	for _, axis := range []struct {
		name, text string
		target     **int64
	}{
		{"cpus", options.cpus, &request.VCPUCount}, {"memory", options.memory, &request.MemoryBytes}, {"disk", options.disk, &request.WorkspaceBytes},
	} {
		if !present[axis.name] {
			continue
		}
		var value int64
		var err error
		if axis.name == "cpus" {
			value, err = strconv.ParseInt(axis.text, 10, 64)
		} else {
			value, err = parseByteSize(axis.text)
		}
		if err != nil || value < 1 {
			unit := "byte size (bytes, KiB/MiB/GiB, or k/m/g)"
			if axis.name == "cpus" {
				unit = "whole vCPU count"
			}
			return nil, fmt.Errorf("SecondBox CLI %s --%s requires a positive %s", flags.Name(), axis.name, unit)
		}
		*axis.target = &value
	}
	return request, nil
}

func parseByteSize(text string) (int64, error) {
	text = strings.ToLower(text)
	multiplier := int64(1)
	for _, unit := range []struct {
		suffix string
		bytes  int64
	}{{"gib", 1 << 30}, {"mib", 1 << 20}, {"kib", 1 << 10}, {"g", 1 << 30}, {"m", 1 << 20}, {"k", 1 << 10}} {
		if strings.HasSuffix(text, unit.suffix) {
			text = strings.TrimSuffix(text, unit.suffix)
			multiplier = unit.bytes
			break
		}
	}
	if text == "" || strings.IndexFunc(text, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return 0, errors.New("SecondBox CLI byte size requires a positive integer and an optional binary unit")
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil || value < 1 || value > math.MaxInt64/multiplier {
		return 0, errors.New("SecondBox CLI byte size is outside the positive int64 range")
	}
	return value * multiplier, nil
}

func sandboxResourceSummary(resources sb.SandboxResources) string {
	return fmt.Sprintf("%d vCPU, %d memory bytes, %d Workspace bytes", resources.VCPUCount, resources.MemoryBytes, resources.WorkspaceBytes)
}

// sandboxCreationError preserves the typed API problem while retaining the
// caller's Profile for an actionable presentation hint.
type sandboxCreationError struct {
	cause   error
	profile string
}

func (failure *sandboxCreationError) Error() string { return failure.cause.Error() }
func (failure *sandboxCreationError) Unwrap() error { return failure.cause }

func (failure *commandPresentationError) hint() string {
	var creation *sandboxCreationError
	var api *sb.APIError
	if errors.As(failure.cause, &creation) && errors.As(creation.cause, &api) && api.Problem != nil && api.Problem.Ceiling != nil {
		ceiling := api.Problem.Ceiling
		var axes []string
		for _, axis := range []struct {
			flag  string
			bound *int64
		}{{"--cpus", ceiling.VCPUCount}, {"--memory", ceiling.MemoryBytes}, {"--disk", ceiling.WorkspaceBytes}} {
			if axis.bound != nil {
				value := *axis.bound
				// Cold-boot disk requests round up. Suggest the largest power of two
				// within a finite ceiling so the suggested retry can actually succeed.
				if axis.flag == "--disk" && api.Problem.Code == sb.ProblemCodeResourcesExceedProfile && value > 0 {
					value = int64(1) << (bits.Len64(uint64(value)) - 1)
				}
				axes = append(axes, fmt.Sprintf("%s %d", axis.flag, value))
			}
		}
		switch api.Problem.Code {
		case sb.ProblemCodeResourcesFixedByProfile:
			return fmt.Sprintf("Profile %q has a fixed size for snapshot_resume. Retry with %s, or omit resource flags.", creation.profile, strings.Join(axes, " "))
		case sb.ProblemCodeResourcesExceedProfile:
			return fmt.Sprintf("Profile %q ceiling: %s. Retry with these bounded axes or lower: %s; inspect policy with secondbox profiles get --path profileName=%s.", creation.profile, sandboxCeilingSummary(*ceiling), strings.Join(axes, " "), creation.profile)
		}
	}
	return "Run the command with --output plain for a stable diagnostic transcript."
}

func sandboxCeilingSummary(ceiling sb.SandboxResourceRequest) string {
	var bounds []string
	for _, axis := range []struct {
		name  string
		bound *int64
	}{{"vCPU", ceiling.VCPUCount}, {"memory bytes", ceiling.MemoryBytes}, {"Workspace bytes", ceiling.WorkspaceBytes}} {
		if axis.bound != nil {
			bounds = append(bounds, fmt.Sprintf("%d %s", *axis.bound, axis.name))
		}
	}
	return strings.Join(bounds, ", ")
}
