// Package harness defines the Sandbox Host's privileged network execution protocol.
package harness

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"strings"
	"time"
)

type ResourceLimits struct {
	MemoryBytes int64
	NanoCPUs    int64
	PidsLimit   int64
}

type NetworkNamespace struct {
	NamespaceName string
	HostVethName  string
	GuestVethName string
	HostIP        string
	GuestIP       string
	ProxyIP       string
	PlatformIP    string
	PrefixLen     int
}

type ProcessCommand struct {
	Command            string
	Args               []string
	Env                []string
	ExtraFiles         []*os.File
	SeccompProfilePath string
	CleanupPaths       []string
}

type PrivilegedNetworkExecutor interface {
	Prepare(context.Context, string, NetworkNamespace) (string, error)
	Execute(context.Context, PrivilegedNetworkExecutionRequest) (PrivilegedNetworkExecutionResult, error)
	Remove(context.Context, string) error
}

type PrivilegedNetworkExecutionRequest struct {
	CellID      string
	Namespace   NetworkNamespace
	Command     ProcessCommand
	Resources   ResourceLimits
	MaxRuntime  time.Duration
	IdleTimeout time.Duration
}

type PrivilegedNetworkExecutionResult struct {
	Started  bool
	Output   string
	ExitCode int
}

func DeriveNetworkNamespace(cellID, cidr string) (*NetworkNamespace, error) {
	cellID = sanitizeIDPart(cellID)
	if cellID == "" {
		return nil, fmt.Errorf("harness cell ID is required")
	}
	_, network, err := net.ParseCIDR(strings.TrimSpace(cidr))
	if err != nil {
		return nil, fmt.Errorf("parse harness netns cidr: %w", err)
	}
	baseIP := network.IP.To4()
	if baseIP == nil {
		return nil, fmt.Errorf("harness netns cidr must be IPv4")
	}
	ones, bits := network.Mask.Size()
	if bits != 32 || ones > 30 {
		return nil, fmt.Errorf("harness netns cidr must provide at least one /30")
	}
	total := uint32(1) << uint32(32-ones)
	blocks := total / 4
	if blocks == 0 {
		return nil, fmt.Errorf("harness netns cidr must provide at least one /30")
	}
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(cellID))
	block := hash.Sum32() % blocks
	base := binary.BigEndian.Uint32(baseIP) + block*4
	host := make(net.IP, net.IPv4len)
	guest := make(net.IP, net.IPv4len)
	binary.BigEndian.PutUint32(host, base+1)
	binary.BigEndian.PutUint32(guest, base+2)
	suffix := fmt.Sprintf("%08x", hash.Sum32())
	return &NetworkNamespace{
		NamespaceName: "ag-" + suffix,
		HostVethName:  "agh" + suffix[:8],
		GuestVethName: "agg" + suffix[:8],
		HostIP:        host.String(),
		GuestIP:       guest.String(),
		PrefixLen:     30,
	}, nil
}

func sanitizeIDPart(value string) string {
	value = strings.TrimSpace(value)
	var builder strings.Builder
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9':
			builder.WriteRune(char)
		case char == '-', char == '_':
			builder.WriteRune(char)
		}
	}
	return builder.String()
}
