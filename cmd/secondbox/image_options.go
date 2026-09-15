package main

import (
	"flag"
	"fmt"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

type executionImageOptions struct {
	reference string
}

func (options *executionImageOptions) register(flags *flag.FlagSet) {
	flags.StringVar(&options.reference, "image", "", "execution image tag or digest; omission uses Profile assets on create or preserves the Sandbox image on start")
}

func (options executionImageOptions) resolve() (contracts.ExecutionImage, error) {
	image := contracts.ExecutionImage{Reference: options.reference}
	if image.Reference == "" {
		return image, nil
	}
	if err := image.Validate(); err != nil {
		return contracts.ExecutionImage{}, fmt.Errorf("SecondBox CLI execution image: %w", err)
	}
	return image, nil
}
