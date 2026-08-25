package cliui

import (
	"strings"
	"testing"
)

func TestInstallerFormBuildersKeepAuthorityInBoundValues(t *testing.T) {
	workspace := ""
	accepted := false
	advanced := "127.0.0.1:8080"
	purge := ""
	forms := []HuhForm{
		WorkspaceChoiceForm([]Option{{Label: "Dedicated Btrfs", Value: "mount"}, {Label: "Btrfs image", Value: "image"}}, &workspace),
		StandardBundleSelectionForm(&accepted),
		CapacityReviewForm("2 Sandboxes, 8 GiB memory", &accepted),
		AdvancedSettingsForm([]TextBinding{{Title: "API bind", Value: &advanced, Validate: func(value string) error { return nil }}}),
		FinalInstallConfirmationForm("review", &accepted),
		ResumeSelectionForm([]Option{{Label: "operation", Value: "op"}}, &workspace),
		UninstallConfirmationForm("data preserved", &accepted),
		PurgeConfirmationForm("PURGE secondbox", &purge),
	}
	for index, form := range forms {
		if len(form.Groups) != 1 || len(form.Groups[0].Fields) == 0 {
			t.Fatalf("form %d is incomplete: %#v", index, form)
		}
	}
	standardBundles := forms[1].Groups[0].Fields[0]
	if !strings.Contains(standardBundles.Description, "agent-compartment-isolated") || !standardBundles.RequireAffirmative {
		t.Fatalf("standard bundle selection = %#v", standardBundles)
	}
	confirm := forms[4].Groups[0].Fields[0]
	if !confirm.RequireAffirmative || confirm.BoolValue != &accepted {
		t.Fatal("final confirmation is not explicit")
	}
	validator := forms[7].Groups[0].Fields[0].ValidateString
	if validator == nil || validator("yes") == nil || validator("PURGE secondbox") != nil {
		t.Fatal("purge typed confirmation is not exact")
	}
}
