package integration_test

import "github.com/SecondStack-AI/SecondBox/pkg/contracts"

func testExecutionImage() contracts.ExecutionImage {
	return contracts.ExecutionImage{Reference: "registry.example/secondbox/integration-agent:stable"}
}
