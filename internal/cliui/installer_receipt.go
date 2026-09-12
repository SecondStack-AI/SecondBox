package cliui

// InstallerTenancyNext describes the completed install's verified tenancy path.
func InstallerTenancyNext(tenantRef, subjectRef, operation, verifiedProfile string) string {
	if tenantRef == "" {
		return "Record-only smoke: no guest command executed.\nBootstrap tenancy: secondbox-deploy bootstrap-tenancy " + operation
	}
	return "Tenant: " + tenantRef + "; Subject: " + subjectRef + "\nGuest smoke: hello-world execution verified.\nRun: secondbox run " + verifiedProfile + " -- /bin/echo hello"
}
