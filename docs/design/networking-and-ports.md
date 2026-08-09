# Networking and ports

Network policy is immutable ProfileRevision policy enforced by the Runner. A guest cannot weaken it, publish a host port, or discover a Runner address through the public API.

## Outbound policy

The default policy denies all outbound destinations. An explicit allow policy may name domains, destination CIDRs, ports, and protocols. Regardless of allow rules, v1 denies guest loopback escape, unspecified, multicast, private, link-local, carrier-grade NAT, cloud-metadata, Runner-host, control-plane management, and Runner management destinations unless a future separately reviewed policy kind states otherwise.

The Runner owns the guest TAP, bridge forwarding rules, and policy-aware DNS proxy. Each assignment gets a separate nftables table keyed by a collision-resistant instance identity. The table permits established replies, runner DNS on the bridge address, and exact policy destinations, then drops all other guest egress and unsolicited traffic toward the TAP. Protected destination drops precede allow rules, so an overlapping allow cannot override them. The current Firecracker path uses per-TAP firewall isolation on the Runner bridge; it does not create a separate Linux network namespace per Sandbox.

## DNS

DNS resolution is coupled to destination enforcement:

- the guest kernel receives only the Runner bridge DNS address, and nftables permits UDP/TCP port 53 only to that address;
- the proxy forwards accepted queries to the explicitly configured `SECONDBOX_RUNNER_NETWORK_POLICY_DNS_UPSTREAM`;
- a domain allow does not install an address rule at assignment start; the rule appears only after that Sandbox sends an exact allowed-name query through the proxy;
- private, link-local, loopback, unspecified, metadata, and Runner-host answers are rejected;
- allowed domains are normalized and matched as exact names;
- accepted answers are pinned to that Sandbox and policy decision for a bounded TTL;
- connection admission checks the observed destination IP against the pin;
- answer changes before pin expiry are rejected as rebinding;
- direct IP use requires an explicit CIDR unless the same Sandbox first created a live address pin through an observed allowed-domain query;
- rebinding from a public answer to a forbidden address is rejected.

The DNS boundary accepts one IN A or AAAA question per message. Responses must match the transaction and echoed question and succeed without an error RCODE. Only address records owned by the exact question or its bounded, loop-free CNAME chain can create pins; unrelated answer and additional records cannot. Message size, concurrent UDP queries and TCP connections, CNAME depth, and I/O time are bounded. Listener death marks network policy unhealthy and fences active instances.

There is no unrestricted resolver fallback. A DNS outage fails the attempted connection rather than bypassing policy.

## Runner and control-plane isolation

Guest traffic cannot reach Runner listeners, the Runner's control-plane connection, KVM management, host services, or other Sandbox interfaces. Per-TAP firewall and per-assignment DNS pin state prevent cross-Sandbox traffic. Cleanup cancels pin expiry work and removes nftables and TAP state before assignment release; readiness fails if nftables or the UDP/TCP DNS listener is unavailable. A later atomic policy update failure is reported to the Manager and terminates the affected instance because continued enforcement cannot be proven.

## Exposed ports

A ProfileRevision lists approved guest ports, protocols, session duration, and concurrency. A trusted caller requests a session for one named approved port on a ready Sandbox generation and supplies the current Lease. Admission transactionally binds the tenant, subject, pinned ProfileRevision, Lease, assignment fence, generation, named port, protocol, duration, and subject/Profile/port-session limits.

Admission is identical for both Port transports. The single-use credential exists for both and is consumed exactly once against PostgreSQL for both. Only the endpoint the control plane returns and the leg that carries bytes differ.

### Proxied transport

The default `proxied` transport returns an expiring WebSocket endpoint whose single-use signed credential is carried in the URI fragment. Clients remove the fragment from the request URL and pass it as the `secondbox.port.token.<credential>` WebSocket protocol alongside `secondbox.port.v1`; this keeps the credential out of the HTTP path, query, and request log. The control plane atomically consumes the credential before upgrading, then forwards binary WebSocket messages over the Runner's authenticated outbound connection without persisting payload bytes. Credit in each direction bounds live buffered bytes and is returned only after the downstream write succeeds.

The Runner forwards the proxied Port stream to the approved guest-loopback port.

### Direct transport

The direct transport removes the control plane from the Port byte path after admission without weakening Port admission authority. It is useful for sustained and latency-sensitive traffic such as SSH and VS Code Remote-SSH.

A caller receives a direct endpoint only when its application authority holds the exact `sandbox:ports:direct` operation scope. The scope is denied by default and is never implied by `sandbox:ports`. Callers without it receive the proxied endpoint and never learn a Runner address, so rollout is a per-authority grant rather than a deployment-wide switch.

The Runner binds one caller-facing data-plane listener at the explicitly configured `SECONDBOX_RUNNER_DATA_PLANE_LISTEN_ADDRESS` and advertises `SECONDBOX_RUNNER_DATA_PLANE_ADVERTISED_ADDRESS` at registration and heartbeat. The advertised value is administrative capacity evidence of the same class as advertised capacity: it carries no Sandbox identity. An unavailable listener makes the Runner unready and fences active instances, matching the existing network-policy listener rule.

Connection admission proceeds in one order:

1. the caller connects and presents the single-use credential as the first framed message, before any payload byte;
2. the Runner rejects locally, in constant time, on any mismatch against the assignment-bound session state it already holds — session, generation, fencing token, Lease, named port, and deadline — so an unauthenticated peer cannot force control-plane work;
3. the Runner consumes the credential through its existing outbound control connection before forwarding any byte, which costs one control-plane round trip per TCP connection and none per byte;
4. on success the Runner opens the same guest-protocol port stream as the proxied transport and copies bytes bidirectionally with no persistence.

TCP flow control governs the caller-to-Runner leg. The existing guest-protocol credit window is retained on the Runner-to-guest leg, where backpressure must still reach the guest process. The proxy credit protocol does not apply to a direct connection.

### Common properties

The guest-facing half is identical for both transports. A dedicated guest-protocol stream dials only the approved `127.0.0.1:<guest-port>` TCP endpoint inside the guest; neither the Runner nor guest agent creates a wildcard listener, host publication, DNAT rule, or fallback route. The guest TAP, bridge, and per-assignment nftables table are not in the Port path, so neither transport changes network policy. TCP and HTTP policies currently use the same binary TCP tunnel.

The public API never returns a bridge address, TAP address, or raw host port, and returns a Runner data-plane address only to an authority holding the exact direct-endpoint grant.

Useful activity starts only when the credential is consumed, not when the session is created or inspected. Client disconnect, terminal delivery, expiry, Lease or generation fencing, operator drain, Instance termination, and Runner disconnect close activity and the session deterministically on both transports; a direct connection's live sockets are closed by the same events, and the Runner returns bounded proof of closure. Without an admitted session, unsolicited inbound traffic toward every TAP remains denied.

UDP, port ranges, public unauthenticated sharing, and ungranted direct access are unsupported. UDP and port ranges require kernel-path forwarding and a flow-lifetime model with no analogue in the current connection-scoped session semantics.

## Evidence

Network decisions emit fixed-shape audit evidence containing request, Sandbox, generation, policy revision, normalized destination class, decision, and reason. Logs do not record credentials, full request bodies, DNS payload contents beyond bounded destination evidence, or workspace data.

Both transports retain the same payload-free session accounting and fixed-shape Runner evidence at admitted open and close. Neither transport persists Port payloads or promises payload reconstruction, and no evidence record can contain a payload byte, a credential, a fencing token, or a Runner address.

See [Profiles and authorization](profiles-and-authorization.md), [API conventions](api-conventions.md), [Security](security.md), and [Recovery and reconciliation](recovery-and-reconciliation.md).
