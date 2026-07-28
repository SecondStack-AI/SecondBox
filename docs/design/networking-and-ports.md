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

Guest traffic cannot reach Runner listeners, the Runner's control-plane connection, KVM management, object-store credentials, host services, or other Sandbox interfaces. Per-TAP firewall and per-assignment DNS pin state prevent cross-Sandbox traffic. Cleanup cancels pin expiry work and removes nftables and TAP state before assignment release; readiness fails if nftables or the UDP/TCP DNS listener is unavailable. A later atomic policy update failure is reported to the Manager and terminates the affected instance because continued enforcement cannot be proven.

## Exposed ports

A ProfileRevision lists approved guest ports, protocols, session duration, and concurrency. An application with port scope requests a session for one named approved port on a ready Sandbox generation and supplies the current Lease. Admission transactionally binds the Project, service account, pinned ProfileRevision, Lease, assignment fence, generation, named port, protocol, duration, and Project/Profile/port-session limits.

The control plane returns an expiring WebSocket endpoint whose single-use signed credential is carried in the URI fragment. Clients remove the fragment from the request URL and pass it as the `secondbox.port.token.<credential>` WebSocket protocol alongside `secondbox.port.v1`; this keeps the credential out of the HTTP path, query, and request log. The control plane atomically consumes the credential before upgrading, then proxies binary WebSocket messages through durable, fenced Runner Port frames. Credit in each direction bounds retained bytes and is returned only after the downstream write succeeds.

The Runner forwards an admitted Port stream over the existing outbound authenticated control connection. A dedicated guest-protocol stream dials only the approved `127.0.0.1:<guest-port>` TCP endpoint inside the guest; neither the Runner nor guest agent creates a wildcard listener, host publication, DNAT rule, or fallback route. The public API never returns a Runner hostname, Runner IP, bridge address, TAP address, or raw host port. TCP and HTTP policies currently use the same binary TCP tunnel. UDP, port ranges, public unauthenticated sharing, and direct Runner access are unsupported.

Useful activity starts only when the credential is consumed, not when the session is created or inspected. Client disconnect, terminal delivery, expiry, Lease or generation fencing, and Runner disconnect close activity and the session deterministically. Without an admitted session, unsolicited inbound traffic toward every TAP remains denied.

## Evidence

Network decisions emit fixed-shape audit evidence containing request, Sandbox, generation, policy revision, normalized destination class, decision, and reason. Logs do not record credentials, full request bodies, DNS payload contents beyond bounded destination evidence, or workspace data.

See [Profiles and authorization](profiles-and-authorization.md), [API conventions](api-conventions.md), [Security](security.md), and [Recovery and reconciliation](recovery-and-reconciliation.md).
