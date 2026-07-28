package conformance

import "testing"

func TestControlPlaneRunnerSessionConformance(t *testing.T) {
	RunSessionSuite(t, DefaultSessionFactory)
}
