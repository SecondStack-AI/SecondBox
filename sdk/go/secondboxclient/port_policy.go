package secondboxclient

import (
	"context"
	"fmt"
)

// PortPolicyForNumber resolves a numeric guest port without guessing a policy
// name. The public Profile route exposes only its current revision; refuse when
// that is no longer the revision pinned by this Sandbox.
func (handle *SandboxHandle) PortPolicyForNumber(ctx context.Context, port int) (PortPolicy, error) {
	sandbox := handle.Snapshot()
	profile, err := handle.client.GetProfile(ctx, sandbox.Profile)
	if err != nil {
		return PortPolicy{}, err
	}
	if profile.CurrentRevision.ID != sandbox.ProfileRevisionID {
		return PortPolicy{}, fmt.Errorf("SecondBox Port policy for pinned ProfileRevision %s is not available from the current Profile", sandbox.ProfileRevisionID)
	}
	var selected *PortPolicy
	for _, policy := range profile.CurrentRevision.Spec.Ports {
		if policy.Port == int64(port) {
			if selected != nil {
				return PortPolicy{}, fmt.Errorf("SecondBox Port %d has multiple named policies", port)
			}
			copy := policy
			selected = &copy
		}
	}
	if selected == nil {
		return PortPolicy{}, fmt.Errorf("SecondBox Port %d is not exposed by Profile %q", port, sandbox.Profile)
	}
	return *selected, nil
}
