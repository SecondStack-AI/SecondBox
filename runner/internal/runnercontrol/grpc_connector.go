package runnercontrol

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"
	"sync"

	runnerprotocol "github.com/SecondStack-AI/SecondBox/runner/internal/runnerprotocol"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// GRPCConnectorConfig contains the mutually authenticated control-plane endpoint.
type GRPCConnectorConfig struct {
	Address           string
	ServerName        string
	ClientCertificate string
	ClientKey         string
	CertificatePool   string
}

// LoadRunnerProtocolConfigFromEnv loads explicit runner identity and mTLS settings.
func LoadRunnerProtocolConfigFromEnv() (RunnerProtocolConfig, GRPCConnectorConfig, error) {
	required := func(name string) (string, error) {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			return "", fmt.Errorf("SecondBox runner protocol config missing required %s", name)
		}
		return value, nil
	}
	runnerID, err := required("SECONDBOX_RUNNER_ID")
	if err != nil {
		return RunnerProtocolConfig{}, GRPCConnectorConfig{}, err
	}
	poolID, err := required("SECONDBOX_RUNNER_POOL_ID")
	if err != nil {
		return RunnerProtocolConfig{}, GRPCConnectorConfig{}, err
	}
	softwareVersion, err := required("SECONDBOX_RUNNER_SOFTWARE_VERSION")
	if err != nil {
		return RunnerProtocolConfig{}, GRPCConnectorConfig{}, err
	}
	address, err := required("SECONDBOX_RUNNER_CONTROL_PLANE_ADDRESS")
	if err != nil {
		return RunnerProtocolConfig{}, GRPCConnectorConfig{}, err
	}
	serverName, err := required("SECONDBOX_RUNNER_CONTROL_PLANE_SERVER_NAME")
	if err != nil {
		return RunnerProtocolConfig{}, GRPCConnectorConfig{}, err
	}
	clientCertificate, err := required("SECONDBOX_RUNNER_CLIENT_CERTIFICATE")
	if err != nil {
		return RunnerProtocolConfig{}, GRPCConnectorConfig{}, err
	}
	clientKey, err := required("SECONDBOX_RUNNER_CLIENT_KEY")
	if err != nil {
		return RunnerProtocolConfig{}, GRPCConnectorConfig{}, err
	}
	certificatePool, err := required("SECONDBOX_RUNNER_CONTROL_PLANE_CA")
	if err != nil {
		return RunnerProtocolConfig{}, GRPCConnectorConfig{}, err
	}

	return RunnerProtocolConfig{
			RunnerID:        runnerID,
			RunnerPoolID:    poolID,
			SoftwareVersion: softwareVersion,
			ProtocolMinimum: 1,
			ProtocolMaximum: 1,
			MandatoryFeatures: []runnerprotocol.RunnerFeature{
				runnerprotocol.RunnerFeature_RUNNER_FEATURE_EXEC_STREAMING,
				runnerprotocol.RunnerFeature_RUNNER_FEATURE_FILE_STREAMING,
				runnerprotocol.RunnerFeature_RUNNER_FEATURE_PTY,
				runnerprotocol.RunnerFeature_RUNNER_FEATURE_EVIDENCE,
				runnerprotocol.RunnerFeature_RUNNER_FEATURE_CHECKPOINT,
				runnerprotocol.RunnerFeature_RUNNER_FEATURE_PORT_PROXY,
			},
		}, GRPCConnectorConfig{
			Address:           address,
			ServerName:        serverName,
			ClientCertificate: clientCertificate,
			ClientKey:         clientKey,
			CertificatePool:   certificatePool,
		}, nil
}

// GRPCConnector creates one outbound runner stream over mutually authenticated TLS.
type GRPCConnector struct {
	address    string
	tlsConfig  *tls.Config
	mu         sync.Mutex
	connection *grpc.ClientConn
}

// NewGRPCConnector validates runner credentials before any network operation.
func NewGRPCConnector(config GRPCConnectorConfig) (*GRPCConnector, error) {
	for name, value := range map[string]string{
		"control-plane address":               config.Address,
		"control-plane server name":           config.ServerName,
		"runner client certificate":           config.ClientCertificate,
		"runner client key":                   config.ClientKey,
		"control-plane certificate authority": config.CertificatePool,
	} {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("SecondBox runner mTLS config requires %s", name)
		}
	}
	certificate, err := tls.LoadX509KeyPair(config.ClientCertificate, config.ClientKey)
	if err != nil {
		return nil, fmt.Errorf("SecondBox runner mTLS client credential: %w", err)
	}
	certificateAuthority, err := os.ReadFile(config.CertificatePool)
	if err != nil {
		return nil, fmt.Errorf("SecondBox runner mTLS control-plane CA: %w", err)
	}
	rootCAs := x509.NewCertPool()
	if !rootCAs.AppendCertsFromPEM(certificateAuthority) {
		return nil, fmt.Errorf("SecondBox runner mTLS control-plane CA has no certificates")
	}
	return &GRPCConnector{
		address: strings.TrimSpace(config.Address),
		tlsConfig: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			ServerName:   strings.TrimSpace(config.ServerName),
			Certificates: []tls.Certificate{certificate},
			RootCAs:      rootCAs,
		},
	}, nil
}

// Connect opens the generated RunnerControl stream through the configured mTLS channel.
func (c *GRPCConnector) Connect(ctx context.Context) (RunnerProtocolStream, error) {
	if c == nil {
		return nil, fmt.Errorf("SecondBox runner gRPC connector is required")
	}
	connection, err := grpc.NewClient(
		c.address,
		grpc.WithTransportCredentials(credentials.NewTLS(c.tlsConfig.Clone())),
	)
	if err != nil {
		return nil, fmt.Errorf("SecondBox runner gRPC client: %w", err)
	}
	stream, err := runnerprotocol.NewRunnerControlClient(connection).Connect(ctx)
	if err != nil {
		connection.Close()
		return nil, fmt.Errorf("SecondBox runner gRPC stream: %w", err)
	}
	c.mu.Lock()
	c.connection = connection
	c.mu.Unlock()
	return stream, nil
}

// Close releases the current runner protocol connection.
func (c *GRPCConnector) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	connection := c.connection
	c.connection = nil
	c.mu.Unlock()
	if connection == nil {
		return nil
	}
	return connection.Close()
}
