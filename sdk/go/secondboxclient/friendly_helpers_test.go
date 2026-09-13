package secondboxclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPortPolicyRequiresPinnedUnambiguousNumber(t *testing.T) {
	for _, test := range []struct{ name, revision, ports, want string }{
		{"pinned", "pinned", `[{"name":"http","port":3000}]`, ""},
		{"revised", "newer", `[{"name":"http","port":3000}]`, "pinned ProfileRevision"},
		{"missing", "pinned", `[]`, "not exposed"},
		{"ambiguous", "pinned", `[{"name":"one","port":3000},{"name":"two","port":3000}]`, "multiple"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, `{"currentRevision":{"id":"`+test.revision+`","spec":{"ports":`+test.ports+`}}}`)
			}))
			defer server.Close()
			client, err := NewSecondBoxSubjectClient(server.URL, "token", "tenant", "subject", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			policy, err := NewSandboxHandle(client, Sandbox{Profile: "coding", ProfileRevisionID: "pinned"}).PortPolicyForNumber(t.Context(), 3000)
			if test.want == "" {
				if err != nil || policy.Name != "http" {
					t.Fatalf("policy=%v err=%v", policy, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestPortForwardReportsPresentationAndCleanupFailures(t *testing.T) {
	var sessionClosed, leaseReleased bool
	leaseExpiry := time.Now().Add(15 * time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/leases"):
			_ = json.NewEncoder(w).Encode(Lease{ID: "lease-1", ExpiresAt: leaseExpiry})
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/port-sessions"):
			var request CreatePortSessionRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if request.DurationSeconds < 1 || time.Now().Add(time.Duration(request.DurationSeconds)*time.Second).After(leaseExpiry) {
				http.Error(w, `{"code":"lease_inactive"}`, http.StatusConflict)
				return
			}
			_, _ = io.WriteString(w, `{"id":"port-1"}`)
		case r.Method == "DELETE" && strings.Contains(r.URL.Path, "/port-sessions/"):
			sessionClosed = true
			w.WriteHeader(500)
			_, _ = io.WriteString(w, `{"code":"cleanup_failed"}`)
		case r.Method == "DELETE" && strings.Contains(r.URL.Path, "/leases/"):
			leaseReleased = true
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := NewSecondBoxSubjectClient(server.URL, "token", "tenant", "subject", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	handle := NewSandboxHandle(client, Sandbox{ID: "sbx-1", Generation: 1})
	outputFailure := errors.New("output failed")
	err = handle.ForwardPort(context.Background(), listener, PortPolicy{Name: "http", MaximumSessionSeconds: 86400}, func(PortSession) error { return outputFailure })
	if !errors.Is(err, outputFailure) || !strings.Contains(err.Error(), "status=500") || !sessionClosed || !leaseReleased {
		t.Fatalf("err=%v sessionClosed=%v leaseReleased=%v", err, sessionClosed, leaseReleased)
	}
	if connection, err := net.Dial("tcp", listener.Addr().String()); err == nil {
		connection.Close()
		t.Fatal("listener remained open")
	}
}

func TestForwardSessionUsesObservedLeaseExpiryAndPolicy(t *testing.T) {
	var duration int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request CreatePortSessionRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		duration = request.DurationSeconds
		_, _ = io.WriteString(w, `{"id":"port-1"}`)
	}))
	defer server.Close()
	client, err := NewSecondBoxSubjectClient(server.URL, "token", "tenant", "subject", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	handle := NewSandboxHandle(client, Sandbox{ID: "sbx-1", Generation: 1})
	keeper := newLeaseKeeper(client, Lease{ID: "lease-1", ExpiresAt: time.Now().Add(10 * time.Second)}, time.Minute, time.Second)
	for _, test := range []struct {
		remaining      time.Duration
		maximum, upper int64
	}{
		{10 * time.Second, 86400, 9},
		{30 * time.Second, 86400, 29}, // The next connection observes a renewed grant.
		{30 * time.Second, 5, 5},
	} {
		keeper.lease.ExpiresAt = time.Now().Add(test.remaining)
		if _, err := handle.createForwardSession(t.Context(), PortPolicy{Name: "http", MaximumSessionSeconds: test.maximum}, keeper); err != nil {
			t.Fatal(err)
		}
		if duration < test.upper-1 || duration > test.upper {
			t.Fatalf("duration=%d upper=%d", duration, test.upper)
		}
	}
	keeper.lease.ExpiresAt = time.Now().Add(time.Second)
	if _, err := handle.createForwardSession(t.Context(), PortPolicy{Name: "http", MaximumSessionSeconds: 86400}, keeper); err == nil {
		t.Fatal("accepted exhausted Lease")
	}
}
