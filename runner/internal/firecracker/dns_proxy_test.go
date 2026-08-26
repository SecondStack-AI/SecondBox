package firecracker

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func TestValidateDNSResponseRejectsUnrelatedAnswerInjection(t *testing.T) {
	query, validated := testDNSQuery(t, 41, "api.example.com.", dnsmessage.TypeA)
	_ = query
	response := testDNSResponse(t, 41, validated.question, func(builder *dnsmessage.Builder) {
		testAddA(t, builder, "attacker.example.", [4]byte{8, 8, 8, 8}, 60)
	})
	if _, _, err := validateDNSResponse(validated, response); err == nil ||
		!strings.Contains(err.Error(), "validated owner chain") {
		t.Fatalf("unrelated answer validation error = %v", err)
	}

	response = testDNSResponse(t, 41, validated.question, func(builder *dnsmessage.Builder) {
		testAddA(t, builder, "api.example.com.", [4]byte{93, 184, 216, 34}, 30)
		testAddA(t, builder, "attacker.example.", [4]byte{8, 8, 8, 8}, 60)
	})
	addresses, ttl, err := validateDNSResponse(validated, response)
	if err != nil {
		t.Fatal(err)
	}
	if len(addresses) != 1 || addresses[0].String() != "93.184.216.34" || ttl != 30*time.Second {
		t.Fatalf("validated addresses = %v ttl=%s", addresses, ttl)
	}
}

func TestValidateDNSResponseRequiresTransactionAndExactQuestion(t *testing.T) {
	_, validated := testDNSQuery(t, 42, "api.example.com.", dnsmessage.TypeA)
	mismatchedID := testDNSResponse(t, 43, validated.question, func(builder *dnsmessage.Builder) {
		testAddA(t, builder, "api.example.com.", [4]byte{93, 184, 216, 34}, 30)
	})
	if _, _, err := validateDNSResponse(validated, mismatchedID); err == nil {
		t.Fatal("mismatched transaction ID was accepted")
	}
	otherName := dnsmessage.MustNewName("other.example.")
	mismatchedQuestion := testDNSResponse(t, 42, dnsmessage.Question{
		Name: otherName, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET,
	}, func(builder *dnsmessage.Builder) {
		testAddA(t, builder, "other.example.", [4]byte{93, 184, 216, 34}, 30)
	})
	if _, _, err := validateDNSResponse(validated, mismatchedQuestion); err == nil {
		t.Fatal("mismatched response question was accepted")
	}
}

func TestValidateDNSResponseFollowsBoundedCNAMEChain(t *testing.T) {
	_, validated := testDNSQuery(t, 44, "api.example.com.", dnsmessage.TypeA)
	response := testDNSResponse(t, 44, validated.question, func(builder *dnsmessage.Builder) {
		alias := dnsmessage.MustNewName("edge.example.net.")
		if err := builder.CNAMEResource(
			dnsmessage.ResourceHeader{
				Name: dnsmessage.MustNewName("api.example.com."),
				Type: dnsmessage.TypeCNAME, Class: dnsmessage.ClassINET, TTL: 20,
			},
			dnsmessage.CNAMEResource{CNAME: alias},
		); err != nil {
			t.Fatal(err)
		}
		testAddA(t, builder, "edge.example.net.", [4]byte{93, 184, 216, 34}, 15)
	})
	addresses, ttl, err := validateDNSResponse(validated, response)
	if err != nil {
		t.Fatal(err)
	}
	if len(addresses) != 1 || addresses[0].String() != "93.184.216.34" || ttl != 15*time.Second {
		t.Fatalf("CNAME addresses = %v ttl=%s", addresses, ttl)
	}
}

// TestValidateDNSResponseForwardsNXDOMAIN proves a genuine name-error
// response passes validation as the strictly empty negative answer - strict
// resolvers fail whole dual-stack lookups when it is refused - while a
// name-error that also carries resolving records stays rejected as injection.
func TestValidateDNSResponseForwardsNXDOMAIN(t *testing.T) {
	_, validated := testDNSQuery(t, 61, "absent.example.com.", dnsmessage.TypeA)
	builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID: 61, Response: true, RCode: dnsmessage.RCodeNameError,
	})
	if err := builder.StartQuestions(); err != nil {
		t.Fatal(err)
	}
	if err := builder.Question(validated.question); err != nil {
		t.Fatal(err)
	}
	response, err := builder.Finish()
	if err != nil {
		t.Fatal(err)
	}
	addresses, ttl, err := validateDNSResponse(validated, response)
	if err != nil {
		t.Fatalf("NXDOMAIN response was rejected: %v", err)
	}
	if len(addresses) != 0 || ttl != 0 {
		t.Fatalf("NXDOMAIN forwarded with pins: %v ttl=%s", addresses, ttl)
	}

	contradictory := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID: 61, Response: true, RCode: dnsmessage.RCodeNameError,
	})
	if err := contradictory.StartQuestions(); err != nil {
		t.Fatal(err)
	}
	if err := contradictory.Question(validated.question); err != nil {
		t.Fatal(err)
	}
	if err := contradictory.StartAnswers(); err != nil {
		t.Fatal(err)
	}
	testAddA(t, &contradictory, "absent.example.com.", [4]byte{192, 0, 2, 7}, 30)
	answered, err := contradictory.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := validateDNSResponse(validated, answered); err == nil {
		t.Fatal("name-error response carrying answers was accepted")
	}
}

func TestDNSMessageBoundsAndWorkerCap(t *testing.T) {
	if _, err := validateDNSQuery(make([]byte, runnerDNSMaximumMessageBytes+1)); err == nil {
		t.Fatal("oversized query was accepted")
	}
	if _, err := readDNSFrame(strings.NewReader("\x10\x01")); err == nil {
		t.Fatal("oversized TCP frame was accepted")
	}
	proxy := &runnerDNSProxy{workers: make(chan struct{}, runnerDNSMaximumConcurrent)}
	for range runnerDNSMaximumConcurrent {
		if !proxy.acquireWorker() {
			t.Fatal("worker capacity exhausted early")
		}
	}
	if proxy.acquireWorker() {
		t.Fatal("worker cap admitted an extra query")
	}
	for range runnerDNSMaximumConcurrent {
		proxy.releaseWorker()
	}
}

func TestRunnerDNSProxyListenerFailureBecomesUnhealthy(t *testing.T) {
	failed := make(chan error, 1)
	proxy := &runnerDNSProxy{
		listen:   netip.MustParseAddr("127.0.0.1"),
		upstream: netip.MustParseAddrPort("127.0.0.1:5353"),
		observe: func(_ context.Context, _, _ string, _ []netip.Addr, _ time.Duration) error {
			return nil
		},
		onFailure: func(err error) { failed <- err },
		listenUDP: func(string, *net.UDPAddr) (*net.UDPConn, error) {
			return net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
		},
		listenTCP: func(string, string) (net.Listener, error) {
			return net.Listen("tcp", "127.0.0.1:0")
		},
	}
	if err := proxy.Start(); err != nil {
		t.Fatal(err)
	}
	if err := proxy.udp.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-failed:
		if !strings.Contains(err.Error(), "UDP listener failed") {
			t.Fatalf("listener failure = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("listener failure was not propagated")
	}
	if err := proxy.Health(); err == nil {
		t.Fatal("dead DNS proxy remained healthy")
	}
	if err := proxy.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("close failed proxy: %v", err)
	}
}

func TestRunnerDNSProxyReturnsRefusedWhenObservationIsDenied(t *testing.T) {
	query, validated := testDNSQuery(t, 45, "api.example.com.", dnsmessage.TypeA)
	upstreamResponse := testDNSResponse(t, 45, validated.question, func(builder *dnsmessage.Builder) {
		testAddA(t, builder, "api.example.com.", [4]byte{93, 184, 216, 34}, 30)
	})
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	upstreamAddress, err := netip.ParseAddrPort(upstream.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	upstreamDone := make(chan error, 1)
	go func() {
		connection, acceptErr := upstream.Accept()
		if acceptErr != nil {
			upstreamDone <- acceptErr
			return
		}
		defer connection.Close()
		if _, readErr := readDNSFrame(connection); readErr != nil {
			upstreamDone <- readErr
			return
		}
		upstreamDone <- writeDNSFrame(connection, upstreamResponse)
	}()

	downstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer downstream.Close()
	proxy := &runnerDNSProxy{
		upstream: upstreamAddress,
		observe: func(context.Context, string, string, []netip.Addr, time.Duration) error {
			return errors.New("DNS policy denied the observed answer")
		},
	}
	proxyDone := make(chan error, 1)
	go func() {
		connection, acceptErr := downstream.Accept()
		if acceptErr != nil {
			proxyDone <- acceptErr
			return
		}
		proxyDone <- proxy.handleTCP(connection)
	}()
	client, err := net.DialTimeout("tcp", downstream.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := writeDNSFrame(client, query); err != nil {
		t.Fatal(err)
	}
	response, err := readDNSFrame(client)
	if err != nil {
		t.Fatal(err)
	}
	var parser dnsmessage.Parser
	header, err := parser.Start(response)
	if err != nil {
		t.Fatal(err)
	}
	if !header.Response || header.RCode != dnsmessage.RCodeRefused {
		t.Fatalf("policy denial DNS header = %#v", header)
	}
	if err := <-upstreamDone; err != nil {
		t.Fatalf("upstream exchange: %v", err)
	}
	if err := <-proxyDone; err != nil {
		t.Fatalf("proxy request: %v", err)
	}
}

func testDNSQuery(t *testing.T, id uint16, name string, recordType dnsmessage.Type) ([]byte, dnsValidatedQuestion) {
	t.Helper()
	question := dnsmessage.Question{
		Name: dnsmessage.MustNewName(name), Type: recordType, Class: dnsmessage.ClassINET,
	}
	builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id})
	if err := builder.StartQuestions(); err != nil {
		t.Fatal(err)
	}
	if err := builder.Question(question); err != nil {
		t.Fatal(err)
	}
	message, err := builder.Finish()
	if err != nil {
		t.Fatal(err)
	}
	validated, err := validateDNSQuery(message)
	if err != nil {
		t.Fatal(err)
	}
	return message, validated
}

func testDNSResponse(
	t *testing.T,
	id uint16,
	question dnsmessage.Question,
	addAnswers func(*dnsmessage.Builder),
) []byte {
	t.Helper()
	builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID: id, Response: true, RCode: dnsmessage.RCodeSuccess,
	})
	if err := builder.StartQuestions(); err != nil {
		t.Fatal(err)
	}
	if err := builder.Question(question); err != nil {
		t.Fatal(err)
	}
	if err := builder.StartAnswers(); err != nil {
		t.Fatal(err)
	}
	addAnswers(&builder)
	message, err := builder.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func testAddA(t *testing.T, builder *dnsmessage.Builder, owner string, address [4]byte, ttl uint32) {
	t.Helper()
	if err := builder.AResource(
		dnsmessage.ResourceHeader{
			Name: dnsmessage.MustNewName(owner),
			Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: ttl,
		},
		dnsmessage.AResource{A: address},
	); err != nil {
		t.Fatal(err)
	}
}
