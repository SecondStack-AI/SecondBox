package main

import (
	"fmt"
	"math"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func validateFleetConfig(cfg fleetConfig) error {
	for name, value := range map[string]string{
		"pool": cfg.Pool, "region": cfg.Region, "groupName": cfg.GroupName, "fleetRef": cfg.FleetRef,
	} {
		if value == "" || strings.TrimSpace(value) != value {
			return fmt.Errorf("SecondBox fleet configuration %s is required without surrounding whitespace", name)
		}
	}
	if !regexp.MustCompile(`^[0-9]{12}$`).MatchString(cfg.Enrollment.AccountID) {
		return fmt.Errorf("SecondBox fleet configuration enrollment.accountId must contain 12 digits")
	}
	if !regexp.MustCompile(`^[A-Za-z0-9+=,.@_-]{1,64}$`).MatchString(cfg.Enrollment.WorkerRoleName) {
		return fmt.Errorf("SecondBox fleet configuration enrollment.workerRoleName must be an IAM role name")
	}
	if cfg.Enrollment.LifetimeSeconds < 1 || cfg.Enrollment.LifetimeSeconds > int64((24*time.Hour)/time.Second) {
		return fmt.Errorf("SecondBox fleet configuration enrollment.lifetimeSeconds must be between 1 and 86400")
	}
	for name, seconds := range map[string]int64{"pollSeconds": cfg.PollSeconds, "enrollment.windowSeconds": cfg.Enrollment.WindowSeconds} {
		if seconds < 1 || seconds > math.MaxInt64/int64(time.Second) {
			return fmt.Errorf("SecondBox fleet configuration %s must be a positive representable duration", name)
		}
	}
	_, port, err := net.SplitHostPort(cfg.Enrollment.ListenAddress)
	number, portErr := strconv.ParseUint(port, 10, 16)
	if err != nil || portErr != nil || number == 0 {
		return fmt.Errorf("SecondBox fleet configuration enrollment.listenAddress must have a valid TCP port")
	}
	for name, endpoint := range map[string]string{"controlPlaneUrl": cfg.ControlPlaneURL, "enrollment.gatewayIssuerUrl": cfg.Enrollment.GatewayIssuerURL} {
		parsed, err := url.Parse(endpoint)
		if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
			return fmt.Errorf("SecondBox fleet configuration %s must be an HTTPS base URL without credentials, query or fragment", name)
		}
		if parsed.Port() != "" {
			port, err := strconv.ParseUint(parsed.Port(), 10, 16)
			if err != nil || port == 0 {
				return fmt.Errorf("SecondBox fleet configuration %s has an invalid TCP port", name)
			}
		}
	}
	return nil
}
