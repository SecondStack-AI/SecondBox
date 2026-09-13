package cliui_test

import (
	"bytes"
	"testing"

	"github.com/SecondStack-AI/SecondBox/internal/cliui"
)

func TestInstallerTenancyReceiptGolden(t *testing.T) {
	for _, test := range []struct{ tenant, subject, want string }{
		{"local", "local-operator", "SecondBox single-host installation complete\n[ok] complete\nNext: Tenant: local; Subject: local-operator\nGuest smoke: hello-world execution verified.\nRun: secondbox run durable-coding -- /bin/echo hello\n"},
		{"", "", "SecondBox single-host installation complete\n[ok] complete\nNext: Record-only smoke: no guest command executed.\nBootstrap tenancy: secondbox-deploy bootstrap-tenancy /operation\n"},
	} {
		var output bytes.Buffer
		renderer := cliui.Renderer{Output: &output, Diagnostic: &output, Capabilities: cliui.ForWriter(&output, &output), OutputMode: cliui.OutputPlain, ColorMode: cliui.ColorNever}
		renderer.Capabilities.Unicode = false
		if err := renderer.WriteSummary(cliui.Summary{Title: "SecondBox single-host installation complete", Status: cliui.StatusComplete, Next: cliui.InstallerTenancyNext(test.tenant, test.subject, "/operation", "durable-coding")}); err != nil {
			t.Fatal(err)
		}
		if output.String() != test.want {
			t.Fatalf("receipt golden:\ngot %q\nwant %q", output.String(), test.want)
		}
	}
}
