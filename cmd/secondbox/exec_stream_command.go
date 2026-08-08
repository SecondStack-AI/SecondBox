package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/SecondStack-AI/SecondBox/internal/cliui"
	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
	"github.com/gorilla/websocket"
)

type execStreamCLIInput struct {
	Type       string `json:"type"`
	DataBase64 string `json:"dataBase64,omitempty"`
	EndOfInput *bool  `json:"endOfInput,omitempty"`
	Bytes      int64  `json:"bytes,omitempty"`
}

func runExecStreamCommand(
	ctx context.Context,
	rawURL string,
	token string,
	tenantRef string,
	subjectRef string,
	args []string,
	input io.Reader,
	output io.Writer,
	httpClient *http.Client,
	dialer *websocket.Dialer,
) error {
	flags := flag.NewFlagSet("exec stream", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	sandboxID := flags.String("sandbox", "", "Sandbox ID")
	generationText := flags.String("generation", "", "current Sandbox generation")
	idempotencyKey := flags.String("idempotency-key", "", "stream creation idempotency key")
	leaseID := flags.String("lease", "", "optional Lease ID")
	requestPath := flags.String("request", "", "streaming exec request JSON file")
	createOnly := flags.Bool("create-only", false, "create the session and print its JSON without attaching")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("SecondBox CLI parse exec stream options: %w", err)
	}
	if len(flags.Args()) != 0 {
		return fmt.Errorf("SecondBox CLI unexpected exec stream arguments: %s", strings.Join(flags.Args(), " "))
	}
	if strings.TrimSpace(rawURL) == "" || strings.TrimSpace(token) == "" ||
		strings.TrimSpace(tenantRef) == "" || strings.TrimSpace(subjectRef) == "" {
		return errors.New(
			"SecondBox CLI exec stream requires --url, --token, --tenant-ref, and --subject-ref" +
				sessionSourceHint,
		)
	}
	if strings.TrimSpace(*sandboxID) == "" ||
		strings.TrimSpace(*generationText) == "" ||
		strings.TrimSpace(*idempotencyKey) == "" ||
		strings.TrimSpace(*requestPath) == "" {
		return errors.New("SecondBox CLI exec stream requires --sandbox, --generation, --idempotency-key, and --request")
	}
	generation, err := strconv.ParseInt(*generationText, 10, 64)
	if err != nil || generation < 1 {
		return errors.New("SecondBox CLI exec stream --generation must be a positive integer")
	}
	requestFile, err := os.Open(*requestPath)
	if err != nil {
		return fmt.Errorf("SecondBox CLI open exec stream request %q: %w", *requestPath, err)
	}
	var request secondboxclient.StreamingExecRequest
	decodeErr := decodeStrictExecStreamJSON(requestFile, &request)
	closeErr := requestFile.Close()
	if err := errors.Join(decodeErr, closeErr); err != nil {
		return fmt.Errorf("SecondBox CLI read exec stream request: %w", err)
	}
	client, err := secondboxclient.NewSecondBoxSubjectClient(
		rawURL, token, tenantRef, subjectRef, httpClient,
	)
	if err != nil {
		return err
	}
	handle := secondboxclient.NewSandboxHandle(client, secondboxclient.Sandbox{
		ID: secondboxclient.OpaqueID(*sandboxID), Generation: generation,
	})
	activity, err := startGuestStreamActivity(ctx, presentationFromContext(ctx, output).renderer, "Negotiate exec stream")
	if err != nil {
		return err
	}
	session, err := handle.CreateExecStream(ctx, request, *idempotencyKey, *leaseID)
	if err != nil {
		return errors.Join(err, completeGuestStreamActivity(activity, cliui.StatusFailed, "session creation failed"))
	}
	if *createOnly {
		if err := completeGuestStreamActivity(activity, cliui.StatusComplete, "session created"); err != nil {
			return err
		}
		return writeExecStreamJSONLine(output, session)
	}
	stream, err := handle.ConnectExecStream(ctx, session, dialer)
	if err != nil {
		return errors.Join(err, completeGuestStreamActivity(activity, cliui.StatusFailed, "attachment failed"))
	}
	defer stream.Close()
	if err := completeGuestStreamActivity(activity, cliui.StatusComplete, "guest stream ready"); err != nil {
		return err
	}
	inputErrors := make(chan error, 1)
	go func() {
		err := pumpExecStreamCLIInput(input, stream)
		if err != nil {
			_ = stream.Close()
		}
		inputErrors <- err
	}()
	for {
		frame, err := stream.Receive()
		if err != nil {
			select {
			case inputErr := <-inputErrors:
				if inputErr != nil {
					return inputErr
				}
			default:
			}
			return err
		}
		if err := writeExecStreamJSONLine(output, frame); err != nil {
			return err
		}
		if frame.StreamOutcomeFrame != nil {
			return nil
		}
	}
}

func pumpExecStreamCLIInput(input io.Reader, stream *secondboxclient.ExecStream) error {
	decoder := json.NewDecoder(bufio.NewReader(input))
	decoder.DisallowUnknownFields()
	for {
		var frame execStreamCLIInput
		if err := decoder.Decode(&frame); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("SecondBox CLI decode exec stream JSONL input: %w", err)
		}
		switch frame.Type {
		case "stdin":
			if frame.Bytes != 0 || frame.EndOfInput == nil {
				return errors.New("SecondBox CLI stdin frame requires endOfInput and cannot carry bytes")
			}
			data, err := base64.StdEncoding.Strict().DecodeString(frame.DataBase64)
			if err != nil {
				return errors.New("SecondBox CLI stdin frame dataBase64 is not canonical base64")
			}
			if err := stream.SendInputFrame(data, *frame.EndOfInput); err != nil {
				return err
			}
		case "credit":
			if frame.DataBase64 != "" || frame.EndOfInput != nil || frame.Bytes < 1 {
				return errors.New("SecondBox CLI credit frame requires positive bytes only")
			}
			if err := stream.GrantOutput(frame.Bytes); err != nil {
				return err
			}
		case "cancel":
			if frame.DataBase64 != "" || frame.EndOfInput != nil || frame.Bytes != 0 {
				return errors.New("SecondBox CLI cancel frame cannot carry data")
			}
			if err := stream.Cancel(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("SecondBox CLI exec stream JSONL frame type %q is unsupported", frame.Type)
		}
	}
}

func decodeStrictExecStreamJSON(reader io.Reader, destination any) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("SecondBox CLI exec stream request contains trailing JSON")
}

func writeExecStreamJSONLine(output io.Writer, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("SecondBox CLI encode exec stream JSONL output: %w", err)
	}
	if _, err := fmt.Fprintf(output, "%s\n", encoded); err != nil {
		return fmt.Errorf("SecondBox CLI write exec stream JSONL output: %w", err)
	}
	return nil
}
