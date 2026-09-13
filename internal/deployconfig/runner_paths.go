package deployconfig

import "path/filepath"

// Same-host paths belong to the packaged Compose mount layout. Remote paths
// belong to the operator's Runner host and must be supplied explicitly.
func (r Runner) identityDirectory() string {
	if r.Placement == "same-host" {
		return "/run/secondbox-runner-identity"
	}
	return r.IdentityDirectory
}

func (r Runner) workspaceRoot() string {
	if r.Placement == "same-host" {
		return "/var/lib/secondbox-runner/workspaces"
	}
	return r.WorkspaceRoot
}

func (r Runner) egressContextConfigPath() string {
	if r.Placement == "same-host" {
		return "/run/secondbox-runner-config/egress-contexts.json"
	}
	return r.EgressContextConfigPath
}

// Compose mounts the storage root once; its existing workspaces child must
// retain that mount identity for jailer hard links and reflink persistence.
func (r Runner) workspaceHostDirectory() string {
	return filepath.Join(r.StateHostDirectory, "workspaces")
}
