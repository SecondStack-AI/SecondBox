package contracts

// RunnerCapabilityPhysicalStorageAdmission indicates that the Runner admits
// Workspace creation and starts against measured filesystem pressure without
// charging the full logical capacity of each Workspace.
const RunnerCapabilityPhysicalStorageAdmission = "physical-storage-admission"

// Runner storage-pressure statuses reported in heartbeats.
const (
	StoragePressureStatusAdmissionDenied = "admission_denied"
	StoragePressureStatusUnavailable     = "unavailable"
)

// StoragePressureRefusesAdmission reports whether a Runner storage-pressure
// status means the Runner refuses new Workspaces and Instances. A Runner over
// its denial threshold, or one whose storage probe failed, stays connected so
// stops can release storage, and placement skips it until a measured
// observation shows it admits again. A Runner without a report is not gated.
func StoragePressureRefusesAdmission(status string) bool {
	return status == StoragePressureStatusAdmissionDenied ||
		status == StoragePressureStatusUnavailable
}
