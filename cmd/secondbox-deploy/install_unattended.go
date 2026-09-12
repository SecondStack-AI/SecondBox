package main

import (
	"context"
	"errors"
	"flag"
	"io"

	"github.com/SecondStack-AI/SecondBox/internal/cliui"
	"github.com/SecondStack-AI/SecondBox/internal/install"
)

func unattendedTenancy(arguments []string) (bool, error) {
	flags := flag.NewFlagSet("install", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	unattended := flags.Bool("unattended", false, "accept the generated development topology")
	tenancy := flags.String("tenancy", "yes", "create local tenancy: yes or no")
	if err := flags.Parse(arguments); err != nil {
		return false, err
	}
	if !*unattended || flags.NArg() != 0 || (*tenancy != "yes" && *tenancy != "no") {
		return false, errors.New("SecondBox installer unattended requires --unattended and --tenancy=yes|no")
	}
	return *tenancy == "yes", nil
}

func runUnattendedInstall(ctx context.Context, arguments []string, renderer cliui.Renderer) error {
	tenancy, err := unattendedTenancy(arguments)
	if err != nil {
		return err
	}
	guide := func(ctx context.Context, renderer cliui.Renderer, facts install.HostFacts, advanced bool) error {
		dependencies := systemGuidedInstallDependencies()
		dependencies.Unattended = true
		dependencies.RunForm = func(_ context.Context, form cliui.Form, _ cliui.FormHandles) error {
			return acceptUnattendedInstallForm(form, tenancy)
		}
		dependencies.Continue = func(ctx context.Context, directory string) error {
			return runInstallResumeWith(ctx, directory, renderer, systemInstallResumeDependencies(renderer))
		}
		return runGuidedInstallWith(ctx, renderer, facts, advanced, dependencies)
	}
	return runInstallPreflightWithGuide(ctx, nil, renderer, func(ctx context.Context) (install.HostFacts, error) {
		return install.Preflight(ctx, install.SystemPreflightProbes())
	}, guide)
}

func acceptUnattendedInstallForm(form cliui.Form, tenancy bool) error {
	spec, ok := form.(cliui.HuhForm)
	if !ok {
		return errors.New("SecondBox installer unattended form is unsupported")
	}
	for _, group := range spec.Groups {
		for _, field := range group.Fields {
			switch field.Kind {
			case cliui.FieldConfirm:
				*field.BoolValue = true
				if group.Title == "Local development tenancy" {
					*field.BoolValue = tenancy
				}
			case cliui.FieldSelect:
				// The guided form supplies the reviewed storage and retention selection.
				if field.StringValue == nil || *field.StringValue == "" {
					return errors.New("SecondBox installer unattended selection is absent")
				}
			default:
				return errors.New("SecondBox installer unattended cannot accept advanced fields")
			}
		}
	}
	return nil
}
