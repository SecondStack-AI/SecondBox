package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/fleet"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/jackc/pgx/v5/pgxpool"
)

type fleetConfig struct {
	Pool                    string           `json:"pool"`
	ControlPlaneURL         string           `json:"controlPlaneUrl"`
	PlatformTokenFile       string           `json:"platformTokenFile"`
	DatabaseURLFile         string           `json:"databaseUrlFile"`
	ControlPlaneCAFile      string           `json:"controlPlaneCaFile"`
	Region                  string           `json:"region"`
	GroupName               string           `json:"groupName"`
	QualificationRun        string           `json:"qualificationRun"`
	MinimumNodes            int              `json:"minimumNodes"`
	MaximumNodes            int              `json:"maximumNodes"`
	NodeVCPUCount           int64            `json:"nodeVcpuCount"`
	NodeMemoryBytes         int64            `json:"nodeMemoryBytes"`
	NodeInstances           int64            `json:"nodeInstances"`
	NodeWorkspaceBytes      int64            `json:"nodeWorkspaceBytes"`
	ScaleInAfterSeconds     int64            `json:"scaleInAfterSeconds"`
	BootstrapTimeoutSeconds int64            `json:"bootstrapTimeoutSeconds"`
	MaximumArchivesPerPass  int              `json:"maximumArchivesPerPass"`
	PollSeconds             int64            `json:"pollSeconds"`
	Enrollment              enrollmentConfig `json:"enrollment"`
}

type enrollmentConfig struct {
	ListenAddress                 string `json:"listenAddress"`
	ServerCertificateFile         string `json:"serverCertificateFile"`
	ServerPrivateKeyFile          string `json:"serverPrivateKeyFile"`
	RunnerCAFile                  string `json:"runnerCaFile"`
	RunnerCAKeyFile               string `json:"runnerCaKeyFile"`
	RunnerEnvironmentFile         string `json:"runnerEnvironmentFile"`
	GatewayEnvironmentFile        string `json:"gatewayEnvironmentFile"`
	EgressContextsFile            string `json:"egressContextsFile"`
	RegistryDirectory             string `json:"registryDirectory"`
	LifetimeSeconds               int64  `json:"lifetimeSeconds"`
	WindowSeconds                 int64  `json:"windowSeconds"`
	AccountID                     string `json:"accountId"`
	WorkerRoleName                string `json:"workerRoleName"`
	GatewayIssuerURL              string `json:"gatewayIssuerUrl"`
	GatewayControlCertificateFile string `json:"gatewayControlCertificateFile"`
	GatewayControlKeyFile         string `json:"gatewayControlKeyFile"`
	GatewayCAFile                 string `json:"gatewayCaFile"`
}

func main() {
	path := flag.String("config", "", "absolute fleet configuration JSON path")
	flag.Parse()
	if err := run(*path); err != nil {
		slog.Error("SecondBox fleet stopped", "error", err)
		os.Exit(1)
	}
}

func run(path string) error {
	if path == "" {
		return errors.New("SecondBox fleet --config is required")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	var cfg fleetConfig
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return err
	}
	if cfg.Region == "" || cfg.PollSeconds < 1 || cfg.Enrollment.ListenAddress == "" || cfg.Enrollment.WindowSeconds < 1 {
		return errors.New("SecondBox fleet cloud region, poll interval and enrollment listener/window are required")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	databaseURL, err := privateFile(cfg.DatabaseURLFile)
	if err != nil {
		return err
	}
	token, err := privateFile(cfg.PlatformTokenFile)
	if err != nil {
		return err
	}
	database, err := pgxpool.New(ctx, strings.TrimSpace(string(databaseURL)))
	if err != nil {
		return err
	}
	defer database.Close()
	lock, err := database.Acquire(ctx)
	if err != nil {
		return err
	}
	defer lock.Release()
	var leader bool
	if err := lock.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,0))`, "secondbox-fleet-"+cfg.Pool).Scan(&leader); err != nil {
		return err
	}
	if !leader {
		return errors.New("SecondBox fleet pool already has an active manager")
	}
	defer lock.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtextextended($1,0))`, "secondbox-fleet-"+cfg.Pool)
	awsConfig, err := config.LoadDefaultConfig(ctx, config.WithRegion(cfg.Region))
	if err != nil {
		return err
	}
	cloud := fleet.AWSCloud{AutoScaling: autoscaling.NewFromConfig(awsConfig), EC2: ec2.NewFromConfig(awsConfig), GroupName: cfg.GroupName, MaximumNodes: cfg.MaximumNodes, QualificationRun: cfg.QualificationRun, Database: database, Pool: cfg.Pool}
	controlClient, err := httpClient(cfg.ControlPlaneCAFile, "", "")
	if err != nil {
		return err
	}
	gatewayClient, err := httpClient(cfg.Enrollment.GatewayCAFile, cfg.Enrollment.GatewayControlCertificateFile, cfg.Enrollment.GatewayControlKeyFile)
	if err != nil {
		return err
	}
	gateway := fleet.HTTPGatewayIssuer{BaseURL: cfg.Enrollment.GatewayIssuerURL, Client: gatewayClient}
	control := fleet.HTTPControlPlane{BaseURL: cfg.ControlPlaneURL, Token: strings.TrimSpace(string(token)), Pool: cfg.Pool, Client: controlClient, Gateway: gateway}
	manager := fleet.Manager{Config: fleet.ManagerConfig{MinimumNodes: cfg.MinimumNodes, MaximumNodes: cfg.MaximumNodes, NodeVCPUCount: cfg.NodeVCPUCount, NodeMemoryBytes: cfg.NodeMemoryBytes, NodeInstances: cfg.NodeInstances, NodeWorkspaceBytes: cfg.NodeWorkspaceBytes, ScaleInAfter: time.Duration(cfg.ScaleInAfterSeconds) * time.Second, MaximumArchivesPerPass: cfg.MaximumArchivesPerPass, BootstrapTimeout: time.Duration(cfg.BootstrapTimeoutSeconds) * time.Second}, Cloud: cloud, Control: control}
	if err := manager.Validate(); err != nil {
		return err
	}
	caPEM, err := os.ReadFile(cfg.Enrollment.RunnerCAFile)
	if err != nil {
		return err
	}
	caKeyPEM, err := privateFile(cfg.Enrollment.RunnerCAKeyFile)
	if err != nil {
		return err
	}
	ca, signer, err := fleet.LoadEnrollmentSigner(caPEM, caKeyPEM)
	if err != nil {
		return err
	}
	runnerEnv, err := privateFile(cfg.Enrollment.RunnerEnvironmentFile)
	if err != nil {
		return err
	}
	gatewayEnv, err := privateFile(cfg.Enrollment.GatewayEnvironmentFile)
	if err != nil {
		return err
	}
	egress, err := privateFile(cfg.Enrollment.EgressContextsFile)
	if err != nil {
		return err
	}
	proofClient := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("SecondBox fleet identity redirects are forbidden")
	}}
	registryFiles, err := loadRegistryFiles(cfg.Enrollment.RegistryDirectory)
	if err != nil {
		return err
	}
	authority := fleet.EnrollmentAuthority{Database: database, Pool: cfg.Pool, Verifier: fleet.AWSIdentityVerifier{AccountID: cfg.Enrollment.AccountID, RoleName: cfg.Enrollment.WorkerRoleName, Region: cfg.Region, Cloud: cloud, Client: proofClient, Now: time.Now, EnrollmentWindow: time.Duration(cfg.Enrollment.WindowSeconds) * time.Second}, Gateway: gateway, CACertificate: ca, CAKey: signer, CAPEM: string(caPEM), Lifetime: time.Duration(cfg.Enrollment.LifetimeSeconds) * time.Second, RunnerEnvironment: string(runnerEnv), GatewayEnvironment: string(gatewayEnv), EgressContexts: string(egress), Now: time.Now}
	authority.RegistryFiles = registryFiles
	server := &http.Server{Addr: cfg.Enrollment.ListenAddress, Handler: authority, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 45 * time.Second, IdleTimeout: time.Minute, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13}}
	serverErrors := make(chan error, 1)
	go func() {
		serverErrors <- server.ListenAndServeTLS(cfg.Enrollment.ServerCertificateFile, cfg.Enrollment.ServerPrivateKeyFile)
	}()
	defer func() {
		stop, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_ = server.Shutdown(stop)
	}()
	ticker := time.NewTicker(time.Duration(cfg.PollSeconds) * time.Second)
	defer ticker.Stop()
	for {
		result, err := manager.Reconcile(ctx, time.Now())
		if err != nil {
			slog.Error("SecondBox fleet reconcile failed", "error", err)
		} else {
			slog.Info("SecondBox fleet reconciled", "action", result.Action, "observedNodes", result.ObservedNodes, "desiredNodes", result.DesiredNodes, "runnerId", result.RunnerID)
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-serverErrors:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-ticker.C:
		}
	}
}

func privateFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("SecondBox fleet private input %s must be a mode-0600 regular file", path)
	}
	return os.ReadFile(path)
}

func httpClient(caPath, certificatePath, keyPath string) (*http.Client, error) {
	ca, err := os.ReadFile(caPath)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, errors.New("SecondBox fleet HTTP certificate authority is invalid")
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots}
	if certificatePath != "" {
		certificate, err := tls.LoadX509KeyPair(certificatePath, keyPath)
		if err != nil {
			return nil, err
		}
		tlsConfig.Certificates = []tls.Certificate{certificate}
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig}, Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("SecondBox fleet HTTP redirects are forbidden")
	}}, nil
}

func loadRegistryFiles(directory string) (map[string]string, error) {
	if !filepath.IsAbs(directory) {
		return nil, errors.New("SecondBox fleet registry directory must be absolute")
	}
	files := map[string]string{}
	total := int64(0)
	err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("SecondBox fleet registry symlinks are forbidden")
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		if !info.Mode().IsRegular() || info.Size() > 128<<10 || total > 512<<10 || len(files) >= 64 {
			return errors.New("SecondBox fleet registry file bounds exceeded")
		}
		content, err := privateFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(directory, path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(relative)] = string(content)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if files["tenants.json"] == "" {
		return nil, errors.New("SecondBox fleet registry tenants.json is required")
	}
	return files, nil
}
