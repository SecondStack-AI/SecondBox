package relayretention

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReportPinsCheckedInWorkloadParameters(t *testing.T) {
	report := NewReport(time.Unix(0, 0), "PostgreSQL test", true)
	if report.Parameters.PTYInputBytes != InteractivePTYInputBytes ||
		report.Parameters.PTYOutputBytes != InteractivePTYOutputBytes ||
		report.Parameters.FileBytes != LargeFileBytes ||
		report.Parameters.PortFrameBytes != RelayPortFrameBytes ||
		report.Parameters.PortTotalBytes != RelayPortTotalBytes ||
		report.Parameters.Cycles != MeasurementCycles {
		t.Fatalf("relay-retention parameters = %#v", report.Parameters)
	}
}

func TestWriteReportRequiresAbsolutePathAndWritesMachineReadableJSON(t *testing.T) {
	report := NewReport(time.Unix(0, 0), "PostgreSQL test", true)
	if err := WriteReport("relative.json", report); err == nil {
		t.Fatal("relative relay-retention output path succeeded")
	}
	path := filepath.Join(t.TempDir(), "result.json")
	if err := WriteReport(path, report); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Report
	if err := json.Unmarshal(content, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.SchemaVersion != 1 || decoded.Postgres != "PostgreSQL test" || !decoded.SeparateFrameRetention || decoded.CompletedAt.IsZero() {
		t.Fatalf("relay-retention result = %#v", decoded)
	}
}
