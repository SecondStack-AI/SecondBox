// secondbox-multirunner-admin performs qualification-only durable Runner control mutations.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/reconcile"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, arguments []string) error {
	if len(arguments) == 0 || arguments[0] != "request-runner-drain" {
		return errors.New("SecondBox multi-Runner admin requires request-runner-drain")
	}
	flags := flag.NewFlagSet("request-runner-drain", flag.ContinueOnError)
	runnerID := flags.String("runner-id", "", "stable Runner identity")
	deadlineSeconds := flags.String("deadline-seconds", "", "bounded drain deadline in seconds")
	if err := flags.Parse(arguments[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(*runnerID) == "" {
		return errors.New("SecondBox multi-Runner admin drain requires exactly one --runner-id")
	}
	seconds, err := strconv.ParseInt(*deadlineSeconds, 10, 64)
	if err != nil || seconds < 1 || seconds > 3600 {
		return errors.New("SecondBox multi-Runner admin --deadline-seconds must be from 1 through 3600")
	}
	databaseURL := strings.TrimSpace(os.Getenv("SECONDBOX_MULTIRUNNER_DATABASE_URL"))
	if databaseURL == "" {
		return errors.New("SecondBox multi-Runner admin missing SECONDBOX_MULTIRUNNER_DATABASE_URL")
	}
	store, err := reconcile.NewPostgresStore(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer store.Close()
	messageID, err := qualificationDrainMessageID()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	return store.RequestRunnerDrain(ctx, *runnerID, &runnerv1.DrainCommand{
		MessageId:      messageID,
		Mode:           runnerv1.DrainMode_DRAIN_MODE_BOUNDED,
		DeadlineUnixMs: uint64(now.Add(time.Duration(seconds) * time.Second).UnixMilli()),
	}, now)
}

func qualificationDrainMessageID() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("SecondBox multi-Runner admin drain identity: %w", err)
	}
	return "qualification-runner-drain-" + hex.EncodeToString(random), nil
}
