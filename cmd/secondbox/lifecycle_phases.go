package main

import (
	"context"

	"github.com/SecondStack-AI/SecondBox/internal/cliui"
)

// startGuestStreamActivity keeps presentation bytes out of guest-owned
// stdout/stderr when diagnostics are redirected. Interactive terminals still
// receive bounded negotiation progress before the guest stream begins.
func startGuestStreamActivity(ctx context.Context, renderer cliui.Renderer, name string) (*cliui.Activity, error) {
	if !renderer.Capabilities.Diagnostic.TTY {
		return nil, nil
	}
	return renderer.StartActivity(ctx, name)
}

func completeGuestStreamActivity(activity *cliui.Activity, status cliui.Status, detail string) error {
	if activity == nil {
		return nil
	}
	return activity.Complete(status, detail)
}

type lifecyclePhase string

const (
	phaseCreate             lifecyclePhase = "create"
	phaseScheduling         lifecyclePhase = "scheduling"
	phaseRunnerAdmission    lifecyclePhase = "Runner admission"
	phaseReadiness          lifecyclePhase = "readiness"
	phaseExecNegotiation    lifecyclePhase = "exec negotiation"
	phaseStop               lifecyclePhase = "stop"
	phaseStart              lifecyclePhase = "start"
	phaseDrain              lifecyclePhase = "drain"
	phaseDelete             lifecyclePhase = "delete"
	phaseSnapshot           lifecyclePhase = "snapshot"
	phaseRelocation         lifecyclePhase = "relocation"
	phaseTerminalAttachment lifecyclePhase = "terminal attachment"
)

// lifecyclePhases documents bounded public-operation milestones presented by
// the CLI. Renderers consume state observed by existing SDK paths; this map
// does not add polling or infer backend state.
var lifecyclePhases = map[string][]lifecyclePhase{
	"run":                {phaseCreate, phaseScheduling, phaseRunnerAdmission, phaseReadiness, phaseExecNegotiation},
	"run --tty":          {phaseCreate, phaseScheduling, phaseRunnerAdmission, phaseReadiness, phaseTerminalAttachment},
	"exec":               {phaseExecNegotiation},
	"shell":              {phaseTerminalAttachment},
	"sandboxes start":    {phaseStart, phaseScheduling, phaseRunnerAdmission, phaseReadiness},
	"sandboxes drain":    {phaseDrain},
	"sandboxes stop":     {phaseStop},
	"sandboxes delete":   {phaseDelete},
	"sandboxes relocate": {phaseRelocation},
	"snapshots create":   {phaseSnapshot},
}
