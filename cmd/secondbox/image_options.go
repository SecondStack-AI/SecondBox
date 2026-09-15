package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

type executionImageOptions struct {
	reference     string
	pullUsername  string
	pullTokenFile string
}

func (options *executionImageOptions) register(flags *flag.FlagSet) {
	flags.StringVar(&options.reference, "image", "", "required client execution image tag or digest reference")
	flags.StringVar(&options.pullUsername, "image-pull-username", "", "registry pull username")
	flags.StringVar(&options.pullTokenFile, "image-pull-token-file", "", "file that contains the registry pull token")
}

func (options executionImageOptions) resolve() (contracts.ExecutionImage, error) {
	image := contracts.ExecutionImage{Reference: options.reference}
	if options.pullUsername == "" && options.pullTokenFile == "" {
		if err := image.Validate(); err != nil {
			return contracts.ExecutionImage{}, fmt.Errorf("SecondBox CLI execution image: %w", err)
		}
		return image, nil
	}
	if options.pullTokenFile == "" {
		return contracts.ExecutionImage{}, errors.New("SecondBox CLI --image-pull-token-file is required with registry credentials")
	}
	content, err := os.ReadFile(options.pullTokenFile)
	if err != nil {
		return contracts.ExecutionImage{}, fmt.Errorf("SecondBox CLI read image pull token: %w", err)
	}
	token := strings.TrimSuffix(string(content), "\n")
	if strings.ContainsAny(token, "\r\n") {
		return contracts.ExecutionImage{}, errors.New("SecondBox CLI image pull token file must contain one line")
	}
	image.PullCredentials = &contracts.RegistryPullCredentials{Username: options.pullUsername, Token: token}
	if err := image.Validate(); err != nil {
		return contracts.ExecutionImage{}, fmt.Errorf("SecondBox CLI execution image: %w", err)
	}
	return image, nil
}
