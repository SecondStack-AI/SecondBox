package cliui

import (
	"fmt"
	"strings"
)

// TextBinding binds one reviewed advanced installer value. The installer owns
// the value and validation; the UI owns only its presentation.
type TextBinding struct {
	Title       string
	Description string
	Value       *string
	Validate    func(string) error
}

func WorkspaceChoiceForm(options []Option, target *string) HuhForm {
	return HuhForm{Groups: []GroupSpec{{Title: "Workspace storage", Fields: []FieldSpec{{Kind: FieldSelect, Title: "Choose durable Workspace storage", Description: "Dedicated XFS/Btrfs mount or a bounded Btrfs filesystem image", StringValue: target, Options: options}}}}}
}

func RetentionChoiceForm(target *string) HuhForm {
	return HuhForm{Groups: []GroupSpec{{Title: "Durable data retention", Fields: []FieldSpec{{Kind: FieldSelect, Title: "Choose data-plane retention", Description: "Operator-owned retention recorded in the accepted plan", StringValue: target, Options: []Option{{Label: "1 day", Value: "86400"}, {Label: "7 days", Value: "604800"}, {Label: "30 days", Value: "2592000"}}}}}}}
}

func StandardBundleSelectionForm(accepted *bool) HuhForm {
	return HuhForm{Groups: []GroupSpec{{Title: "Release-owned standard Profiles", Fields: []FieldSpec{{Kind: FieldConfirm, Title: "Install all three standard Profiles?", Description: "Explicitly select agent-compartment, durable-coding, and agent-compartment-isolated for the guided topology.", BoolValue: accepted, RequireAffirmative: true}}}}}
}

func CapacityReviewForm(summary string, accepted *bool) HuhForm {
	return HuhForm{Groups: []GroupSpec{{Title: "Capacity review", Fields: []FieldSpec{{Kind: FieldConfirm, Title: "Accept this capacity?", Description: Sanitize(summary), BoolValue: accepted, RequireAffirmative: true}}}}}
}

func AdvancedSettingsForm(bindings []TextBinding) HuhForm {
	fields := make([]FieldSpec, 0, len(bindings))
	for _, binding := range bindings {
		fields = append(fields, FieldSpec{Kind: FieldText, Title: binding.Title, Description: binding.Description, StringValue: binding.Value, ValidateString: binding.Validate})
	}
	return HuhForm{Groups: []GroupSpec{{Title: "Advanced network and path settings", Fields: fields}}}
}

func FinalInstallConfirmationForm(review string, accepted *bool) HuhForm {
	return HuhForm{Groups: []GroupSpec{{Title: "Final installation review", Fields: []FieldSpec{{Kind: FieldConfirm, Title: "Install this reviewed plan?", Description: Sanitize(review), BoolValue: accepted, RequireAffirmative: true}}}}}
}

func FinalUpdateConfirmationForm(review string, accepted *bool) HuhForm {
	return HuhForm{Groups: []GroupSpec{{Title: "Final update review", Fields: []FieldSpec{{Kind: FieldConfirm, Title: "Activate this verified release update?", Description: Sanitize(review), BoolValue: accepted, RequireAffirmative: true}}}}}
}

func ResumeSelectionForm(options []Option, target *string) HuhForm {
	return HuhForm{Groups: []GroupSpec{{Title: "Resume installation", Fields: []FieldSpec{{Kind: FieldSelect, Title: "Select an interrupted operation", StringValue: target, Options: options}}}}}
}

func UninstallConfirmationForm(review string, accepted *bool) HuhForm {
	return HuhForm{Groups: []GroupSpec{{Title: "Uninstall services", Fields: []FieldSpec{{Kind: FieldConfirm, Title: "Stop services and preserve durable data?", Description: Sanitize(review), BoolValue: accepted, RequireAffirmative: true}}}}}
}

func PurgeConfirmationForm(expected string, answer *string) HuhForm {
	return HuhForm{Groups: []GroupSpec{{
		Title: "Permanently purge installation-owned resources",
		Fields: []FieldSpec{{
			Kind: FieldText, Title: fmt.Sprintf("Type %s to purge", expected),
			Description: "This cannot be undone.", StringValue: answer,
			ValidateString: func(value string) error {
				if strings.TrimSpace(value) != expected {
					return fmt.Errorf("type %s exactly to continue", expected)
				}
				return nil
			},
		}},
	}}}
}
