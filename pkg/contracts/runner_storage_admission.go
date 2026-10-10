package contracts

// RunnerCapabilityPhysicalStorageAdmission indicates that the Runner admits
// Workspace creation and starts against measured filesystem pressure without
// charging the full logical capacity of each Workspace.
const RunnerCapabilityPhysicalStorageAdmission = "physical-storage-admission"

// StoragePressureStatusAdmissionDenied is the Runner storage-pressure status
// under which the Runner refuses new Workspaces and Instances. The Runner stays
// connected so stops can release storage; placement skips it until it recovers.
const StoragePressureStatusAdmissionDenied = "admission_denied"
