package contracts

import "time"

type WorkspaceStorageObservation struct {
	Status         string                     `json:"status"`
	ObservedAt     *time.Time                 `json:"observedAt,omitempty"`
	AllocatedBytes *int64                     `json:"allocatedBytes,omitempty"`
	Reason         string                     `json:"reason,omitempty"`
	Pressure       StoragePressureObservation `json:"pressure"`
}

type StoragePressureObservation struct {
	Status     string     `json:"status"`
	ObservedAt *time.Time `json:"observedAt,omitempty"`
}
