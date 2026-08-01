package main

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
	scenarioharness "github.com/SecondStack-AI/SecondBox/tests/scenario/harness"
)

// Gate modes. The block is required and its mode explicit, so an operator never
// discovers by accident whether a run asserts or only observes.
const (
	gateObserve = "observe"
	gateEnforce = "enforce"
)

// gateConfig turns the benchmark into a pass/fail gate.
//
// Enforcing only makes sense when a configured limit binds inside the ladder's
// range. The clean refusal paths are the configured runner and subject limits;
// raise them above the ladder so the host binds instead and the deployment
// produces startup failures, not refusals. A host-bound discovery run therefore
// observes, and a config-bound gate run enforces.
type gateConfig struct {
	Mode            string `json:"mode"`
	DeclaredCeiling int    `json:"declaredCeiling"`
}

type gateViolation struct {
	Check   string `json:"check"`
	Cell    string `json:"cell"`
	Message string `json:"message"`
}

func (violation gateViolation) String() string {
	return fmt.Sprintf("%s [%s]: %s", violation.Check, violation.Cell, violation.Message)
}

func cellName(cell cellResult) string {
	return fmt.Sprintf(
		"measurement=%s pattern=%s resident=%d",
		cell.Measurement, cell.Pattern, cell.ResidentPopulation,
	)
}

// evaluateGate applies every check and returns all violations rather than the
// first, so one run names every way it fell short.
func evaluateGate(gate gateConfig, results []cellResult) []gateViolation {
	var violations []gateViolation
	for _, cell := range results {
		below := cell.OfferedArrivals <= gate.DeclaredCeiling

		// (a) Below the declared ceiling the deployment must simply cope.
		if below {
			if len(cell.Refusals) > 0 {
				violations = append(violations, gateViolation{
					Check: "below-ceiling-clean", Cell: cellName(cell),
					Message: fmt.Sprintf(
						"%d offered is within the declared ceiling %d but was refused: %v",
						cell.OfferedArrivals, gate.DeclaredCeiling, cell.Refusals,
					),
				})
			}
			if len(cell.Failures) > 0 {
				violations = append(violations, gateViolation{
					Check: "below-ceiling-clean", Cell: cellName(cell),
					Message: fmt.Sprintf(
						"%d offered is within the declared ceiling %d but failed: %v",
						cell.OfferedArrivals, gate.DeclaredCeiling, cell.Failures,
					),
				})
			}
		}

		// (b) Above it the deployment must shed, not break.
		if !below {
			if len(cell.Refusals) == 0 {
				violations = append(violations, gateViolation{
					Check: "above-ceiling-sheds", Cell: cellName(cell),
					Message: fmt.Sprintf(
						"%d offered exceeds the declared ceiling %d but nothing was refused",
						cell.OfferedArrivals, gate.DeclaredCeiling,
					),
				})
			}
			if len(cell.Failures) > 0 {
				violations = append(violations, gateViolation{
					Check: "above-ceiling-sheds", Cell: cellName(cell),
					Message: fmt.Sprintf(
						"overload produced genuine failures rather than refusals: %v",
						cell.Failures,
					),
				})
			}
		}

		// (c) The measurement must be of the deployment, not of the driver.
		// A shed arrival never reached the deployment at all.
		if cell.ShedArrivals > 0 {
			violations = append(violations, gateViolation{
				Check: "measured-the-deployment", Cell: cellName(cell),
				Message: fmt.Sprintf(
					"the driver shed %d of %d arrivals, so this cell measured "+
						"maximumInFlight rather than the deployment",
					cell.ShedArrivals, cell.OfferedArrivals,
				),
			})
		}
		if cell.PeakOutstandingArrivals > int64(cell.OfferedArrivals) {
			violations = append(violations, gateViolation{
				Check: "measured-the-deployment", Cell: cellName(cell),
				Message: fmt.Sprintf(
					"peak outstanding %d exceeded the %d arrivals offered",
					cell.PeakOutstandingArrivals, cell.OfferedArrivals,
				),
			})
		}
	}
	return violations
}

// countRemainingSandboxes reports how many Sandboxes this qualification still
// owns. Cleanup deletes residents as well as measurement Sandboxes, so a
// finished run must own none: the expected count is zero, not the resident
// population. The driver's own bookkeeping cannot answer this — it cannot see a
// Sandbox it lost track of — so the check goes to the API.
func (driver *lifecycleDriver) countRemainingSandboxes(ctx context.Context) (int, error) {
	remaining := 0
	cursor := ""
	for page := 0; page < 100; page++ {
		parameters := url.Values{
			"metadata": {"qualification=lifecycle"},
			"limit":    {strconv.Itoa(100)},
		}
		if cursor != "" {
			parameters.Set("cursor", cursor)
		}
		listed, err := scenarioharness.RequestJSON[secondboxclient.SandboxPage](
			ctx, driver.client, "listSandboxes",
			secondboxclient.CallOptions{QueryParameters: parameters},
		)
		if err != nil {
			return 0, fmt.Errorf("SecondBox lifecycle Sandbox listing failed: %w", err)
		}
		for _, sandbox := range listed.Items {
			if sandbox.State != secondboxclient.SandboxStateDeleted {
				remaining++
			}
		}
		if listed.NextCursor == nil || *listed.NextCursor == "" {
			return remaining, nil
		}
		cursor = *listed.NextCursor
	}
	return remaining, fmt.Errorf(
		"SecondBox lifecycle Sandbox listing did not terminate within 100 pages",
	)
}
