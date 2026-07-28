package runnercontrol

import (
	"errors"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
)

func TestControlPlaneUpgradeNegotiatesConfiguredRunnerGenerationWindow(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		minimum   uint32
		maximum   uint32
		selected  uint32
		rejection runnerv1.ProtocolRejectionKind
	}{
		{name: "lower_generation_only", minimum: 1, maximum: 1, selected: 1},
		{name: "complete_configured_window", minimum: 1, maximum: 2, selected: 2},
		{
			name: "future_only", minimum: 3, maximum: 3,
			rejection: runnerv1.ProtocolRejectionKind_PROTOCOL_REJECTION_KIND_VERSION_UNSUPPORTED,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			session := NewSession(SessionConfig{
				AuthenticatedRunnerID: "upgrade-runner",
				SupportedVersions:     VersionRange{Minimum: 1, Maximum: 2},
				EnabledFeatures: []runnerv1.RunnerFeature{
					runnerv1.RunnerFeature_RUNNER_FEATURE_EVIDENCE,
				},
				HeartbeatInterval: 10 * time.Second,
				ConnectionID:      "upgrade-connection",
			})
			response, err := session.Accept(helloFrame(
				"upgrade-runner",
				testCase.minimum,
				testCase.maximum,
			))
			if err != nil {
				t.Fatal(err)
			}
			if testCase.rejection != runnerv1.ProtocolRejectionKind_PROTOCOL_REJECTION_KIND_UNSPECIFIED {
				if got := response.GetRejection().GetKind(); got != testCase.rejection {
					t.Fatalf("Runner protocol rejection = %s, want %s", got, testCase.rejection)
				}
				if _, err := session.Accept(
					registrationFrame("upgrade-runner", "upgrade-connection", 1),
				); !errors.Is(err, ErrHelloRequired) {
					t.Fatalf("registration after rejected skew error = %v, want ErrHelloRequired", err)
				}
				return
			}
			if got := response.GetWelcome().GetSelectedVersion(); got != testCase.selected {
				t.Fatalf("selected Runner protocol generation = %d, want %d", got, testCase.selected)
			}
			registration := registrationFrame("upgrade-runner", "upgrade-connection", 1)
			registration.GetRegistration().ProtocolVersion = testCase.selected
			event, err := session.Accept(registration)
			if err != nil || event.Kind != EventRegistration {
				t.Fatalf("negotiated registration event = %#v, error %v", event, err)
			}
		})
	}
}
