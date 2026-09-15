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
	flags.StringVar(&options.reference, "image", "", "execution image tag or digest; required for create, optional for start")
}

func (options executionImageOptions) resolve() (contracts.ExecutionImage, error) {
	image := contracts.ExecutionImage{Reference: options.reference}
	if err := image.Validate(); err != nil {
		return contracts.ExecutionImage{}, fmt.Errorf("SecondBox CLI execution image: %w", err)
	}
	return image, nil
}
