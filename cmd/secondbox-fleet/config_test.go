package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFleetStartupRejectsInvalidEnrollmentBeforeOpeningInfrastructure(t *testing.T) {
	valid := fleetConfig{Pool: "code", Region: "us-east-1", GroupName: "code-workers", FleetRef: "code-fleet", PollSeconds: 10, ControlPlaneURL: "https://control.example:7443", Enrollment: enrollmentConfig{AccountID: "123456789012", WorkerRoleName: "code-workers-role", LifetimeSeconds: 86400, WindowSeconds: 1800, ListenAddress: ":9444", GatewayIssuerURL: "https://issuer.example:9443"}}
	if err := validateFleetConfig(valid); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		field  string
		change func(*fleetConfig)
	}{
		{"pool", func(c *fleetConfig) { c.Pool = "" }},
		{"region", func(c *fleetConfig) { c.Region = "" }},
		{"groupName", func(c *fleetConfig) { c.GroupName = "" }},
		{"fleetRef", func(c *fleetConfig) { c.FleetRef = "" }},
		{"pollSeconds", func(c *fleetConfig) { c.PollSeconds = 0 }},
		{"enrollment.accountId", func(c *fleetConfig) { c.Enrollment.AccountID = "invalid" }},
		{"enrollment.workerRoleName", func(c *fleetConfig) { c.Enrollment.WorkerRoleName = "" }},
		{"enrollment.lifetimeSeconds", func(c *fleetConfig) { c.Enrollment.LifetimeSeconds = 0 }},
		{"enrollment.lifetimeSeconds", func(c *fleetConfig) { c.Enrollment.LifetimeSeconds = 86401 }},
		{"enrollment.windowSeconds", func(c *fleetConfig) { c.Enrollment.WindowSeconds = -1 }},
		{"enrollment.listenAddress", func(c *fleetConfig) { c.Enrollment.ListenAddress = "host:0" }},
		{"controlPlaneUrl", func(c *fleetConfig) { c.ControlPlaneURL = "http://control.example" }},
		{"enrollment.gatewayIssuerUrl", func(c *fleetConfig) { c.Enrollment.GatewayIssuerURL = "" }},
		{"enrollment.gatewayIssuerUrl", func(c *fleetConfig) { c.Enrollment.GatewayIssuerURL = "https://issuer.example?key=secret" }},
	} {
		t.Run(test.field, func(t *testing.T) {
			cfg := valid
			test.change(&cfg)
			encoded, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "fleet.json")
			if err := os.WriteFile(path, encoded, 0600); err != nil {
				t.Fatal(err)
			}
			// No database, files or AWS credentials are provided. The precise
			// startup diagnostic proves validation precedes those dependencies.
			if err := run(path); err == nil || !strings.Contains(err.Error(), test.field) {
				t.Fatalf("startup must reject %s before infrastructure access: %v", test.field, err)
			}
		})
	}
}
