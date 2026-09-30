# Networking and ports

Network policy is immutable ProfileRevision policy enforced by the Runner. A guest cannot weaken it, publish a host port, or discover a Runner address through the public API.

## Outbound policy

A Profile must explicitly select its outbound policy. `deny_all` denies all outbound destinations. An explicit allow policy may name domains, destination CIDRs, ports, and protocols. Regardless of allow rules, v1 denies guest loopback escape, unspecified, multicast, private, link-local, carrier-grade NAT, cloud-metadata, Runner-host, control-plane management, and Runner management destinations except for an explicitly configured logical gateway mapping in the pinned Tenant egress context.

The Runner owns the guest TAP, bridge forwarding rules, and policy-aware DNS proxy. Each assignment gets a separate nftables table keyed by a collision-resistant instance identity. The table permits established replies, runner DNS on the bridge address, and exact policy destinations, then drops all other guest egress and unsolicited traffic toward the TAP. Protected destination drops precede allow rules, so an overlapping allow cannot override them. The current Firecracker path uses per-TAP firewall isolation on the Runner bridge; it does not create a separate Linux network namespace per Sandbox.

The DNS proxy is backend-neutral: it listens on the Runner bridge address for
Firecracker guests and on the per-profile `sbxgv-dns` address for gVisor
guests, and forwards guest questions to the configured upstream, pinning only
answers allowed by the Sandbox policy. It rejects answers resolving to
protected addresses. Strictly empty negative responses - an empty NOERROR and
a validated NXDOMAIN carrying no resolving records - are forwarded to the
guest without pinning anything, because strict resolvers fail whole
dual-stack lookups when the negative half is refused; a name-error response
that carries resolving records stays rejected as injection. A Runner logical-gateway mapping is authorization input for network
policy, not a DNS record: the proxy does not synthesize guest answers for the
logical domain. Network-enabled production deployments must provide and qualify
their own upstream resolution and gateway reachability.

## Backend topologies

The Firecracker backend implements the outbound contract with per-TAP bridge-family firewall
isolation on the Runner bridge, as described above. The gVisor backend implements
the same contract with a different topology: each Instance runs in its own Linux network
namespace connected by a routed veth pair, the shared enforcer renders the identical fail-closed
policy into `inet`-family tables (with a paired `ip`-family NAT table for masqueraded egress),
and the policy-aware DNS proxy listens on a per-profile address of the Runner's `sbxgv-dns`
dummy interface instead of a bridge address. Runners sharing a host network namespace are
separated by an explicit network profile in `0`-`15`. The reserved link-local ranges are
exact: profile `N` owns the `/24` at `169.254.(104+N).0` - `169.254.104.0/24` through
`169.254.119.0/24` across all profiles - carved into per-Instance `/30` subnets (host side
`.1`, guest side `.2`), bounding each profile at 63 concurrent Instances, and its DNS proxy
binds `169.254.99.(53+N)` on `sbxgv-dns`. Operators must keep these ranges, the `gvh` (host) and
`gvg` (guest) veth prefixes, the `sbxgv` network-namespace prefix, and the `169.254.99.*`
proxy addresses free of conflicting host use. Per-Instance teardown removes the namespace, veth, and both policy-table families,
and startup reconciliation sweeps any profile-scoped leftovers, including orphaned NAT-only
tables. DNS pinning, protected-destination precedence, and the deny-all default are identical
across both backends.

At its admitted connection limit, an exec's forwarder closes newly accepted sockets
before connecting to the gateway or sending bytes. Clients can observe an immediate
EOF. This finite safety bound counts open TCP streams, including idle persistent
streams; it does not parse HTTP requests or produce HTTP status codes. The first
capacity refusal per exec emits a bounded diagnostic. Existing relays remain usable
and release their slots when closed. The limit comes from the Assignment, so Subject
policy changes apply only to the next Assignment; see
[connection policy](configurable-limits.md#attributed-connection-policy).

## Tenant contexts

A network-enabled Profile requires a Tenant egress context. Sandbox creation
pins that logical context; admission requires its exact name among the home
Runner's advertised mappings. The Runner resolves Profile gateway names only
inside that context. A mapping permits the configured gateway address and port;
it does not synthesize guest DNS or distribute proxy credentials or CA trust.
Missing contexts fail admission without substituting another mapping.

Because there is no guest DNS for these names, the Runner publishes the
resolved endpoints instead. On the Firecracker and gVisor backends every guest
execution receives the reserved `SECONDBOX_RUNNER_GATEWAYS` environment
variable, which holds space-separated `logicalName=address:port` entries sorted
by logical name and then port, one entry for each resolved logical gateway in
the assignment's compiled policy. The variable is absent when the policy
resolves none. The experimental Microsandbox backend does not use the Runner
guest protocol, so it neither publishes nor reserves the name. The published
endpoints are routing information only, so application wrappers still choose
the proxy variables and the HTTP semantics, and the application host no longer
carries a Runner-host address in its own deployment configuration.

## Attributed execution

Attribution belongs to one exec inside an ordinary, long-lived generation. The
immutable Profile permits one named gateway. Every Assignment of such a Profile
carries that gateway and its connection limit, and the Runner resolves the
operator-configured Unix socket inside the pinned context when the Assignment
starts; an unresolvable route fails the Assignment. The Assignment carries no
execution identity, and its generation keeps its ordinary network policy, Runner
gateways, PTYs, Ports, and file writes.

A buffered or streaming exec admitted with `attributedExecution` opens a window.
Firecracker and gVisor bind a new listener on an ephemeral port of the Runner side
of the Instance interface: the bridge address for Firecracker and the host veth
address for gVisor. The listener's own nftables table admits only that Instance's
interface and guest address and refuses host-local clients. The Runner then
atomically replaces the Instance policy table with one that also admits the
listener, in the same way it applies DNS pin updates, and injects
`SECONDBOX_EXECUTION_GATEWAY=<address:port>` into that exec's environment only.
Callers still cannot supply this reserved name. Each accepted connection receives
an `SBXATTR1` preface derived from the frame fence and the admitted binding:
Tenant, Subject, Sandbox, Instance, Assignment, generation, authorization
reference, and expiry. Guest headers and source addresses are not identity claims,
and the receiving gateway must authenticate the Unix peer before it accepts the
preface.

The window closes when the exec ends, is cancelled, reaches its deadline or
`expiresAt`, or loses its control-plane connection, and when its Instance is
fenced. Closing revokes the listener and its active relays, then removes the
listener from the Instance policy and deletes its listener table. The Instance
keeps running. A gateway connection or identity-preface failure, or a listener
failure, cancels the exec and reports a Runner failure. If the Runner cannot
prove that the rule was removed, it terminates the Instance, as for any failed
policy update. A peer reset, broken pipe, or close after a completed response
ends only that connection. Listener tables are named per exec under a prefix of
the Instance interface, so Instance teardown and Runner startup sweep every
remaining table for a reclaimed interface.

Any process in the Instance can reach an open window's listener, including a
daemon started by an earlier exec, and its traffic carries that exec's identity.
This is an accepted residual risk: the guest never holds real credentials, and
the gateway and Integrations authorize each credentialed request. The attributed
exec also keeps its generation's ordinary routes, including
`SECONDBOX_RUNNER_GATEWAYS`; Integrations deny credential selectors on traffic
that arrives without attribution. See [Threat model](threat-model.md),
[Profiles and authorization](profiles-and-authorization.md), and the
[gateway deployment contract](../operations/deployment.md).

## DNS

DNS resolution is coupled to destination enforcement:

- the guest kernel receives only the Runner's DNS proxy address - the bridge address for Firecracker guests, the per-profile `169.254.99.(53+N)` address on `sbxgv-dns` for gVisor guests - and nftables permits UDP/TCP port 53 only to that address;
- the proxy forwards accepted queries to the explicitly configured `SECONDBOX_RUNNER_NETWORK_POLICY_DNS_UPSTREAM`;
- a domain allow does not install an address rule at assignment start; the rule appears only after that Sandbox sends an exact allowed-name query through the proxy;
- private, link-local, loopback, unspecified, metadata, and Runner-host answers are rejected;
- allowed domains are normalized and matched as exact names;
- accepted answers are pinned to that Sandbox and policy decision for a bounded TTL;
- connection admission checks the observed destination IP against the pin;
- answer changes before pin expiry are rejected as rebinding;
- direct IP use requires an explicit CIDR unless the same Sandbox first created a live address pin through an observed allowed-domain query;
- rebinding from a public answer to a forbidden address is rejected.

The DNS boundary accepts one IN A or AAAA question per message. Responses must match the transaction and echoed question, and carry either a success RCODE or a validated NXDOMAIN with no resolving records: strictly empty negative responses (an empty NOERROR or such an NXDOMAIN) are forwarded to the guest without creating any pin, because strict resolvers fail whole dual-stack lookups when the negative half is refused. Only address records owned by the exact question or its bounded, loop-free CNAME chain can create pins; unrelated answer and additional records cannot, and a name-error response carrying resolving records is rejected as injection. Message size, concurrent UDP queries and TCP connections, CNAME depth, and I/O time are bounded. Listener death marks network policy unhealthy and fences active instances.

There is no unrestricted resolver fallback. A DNS outage fails the attempted connection rather than bypassing policy.

## Runner and control-plane isolation

Guest traffic cannot reach Runner listeners, the Runner's control-plane connection, KVM management, host services, or other Sandbox interfaces. Per-TAP firewall and per-assignment DNS pin state prevent cross-Sandbox traffic. Cleanup cancels pin expiry work and removes nftables and TAP state before assignment release; readiness fails if nftables or the UDP/TCP DNS listener is unavailable. A later atomic policy update failure is reported to the Manager and terminates the affected instance because continued enforcement cannot be proven.

## Exposed ports

A ProfileRevision lists approved guest ports, protocols, session duration, and concurrency. A trusted caller requests a session for one named approved port on a ready Sandbox generation and supplies the current Lease. Admission transactionally binds the tenant, subject, pinned ProfileRevision, Lease, assignment fence, generation, named port, protocol, duration, and subject/Profile/port-session limits.

A PortSession lives while the Lease it was admitted under is renewed. Its requested `durationSeconds`, at most the Port policy's `maximumSessionSeconds`, bounds it and sets `expiresAt`; the Lease's current grant does not. The data-plane sweep ends the session when its Lease is released, lapses, or is fenced. The endpoint credential stays single-use, so a caller that reconnects creates a new PortSession.

A Sandbox has one active Lease, so an application that shares a Sandbox between its own work and a long-lived viewer holds that Lease in one place and mints PortSessions under it: for example, Agent Platform holds the Sandbox's owner Lease, renews it on its own cadence, creates a PortSession per viewer connection, and relays the tunnel to its UI. Viewers never hold a Lease or a SecondBox credential of their own, and releasing the Lease ends every PortSession admitted under it.

Admission is identical for both Port transports. The single-use credential exists for both and is consumed exactly once against PostgreSQL for both. Only the endpoint the control plane returns and the leg that carries bytes differ.

### Proxied transport

The default `proxied` transport returns an expiring WebSocket endpoint whose single-use signed credential is carried in the URI fragment. Clients remove the fragment from the request URL and pass it as the `secondbox.port.token.<credential>` WebSocket protocol alongside `secondbox.port.v1`; this keeps the credential out of the HTTP path, query, and request log. The control plane atomically consumes the credential before upgrading, then forwards binary WebSocket messages over the Runner's authenticated outbound connection without persisting payload bytes. Credit in each direction bounds live buffered bytes and is returned only after the downstream write succeeds. Payload, credit, and sequence state live only in the process that owns the tunnel: no Port byte or credit frame ever reaches PostgreSQL, and a live stream writes nothing to it. PostgreSQL records the session's admission, credential consumption, close, and terminal outcome. An open session holds its Sandbox out of idle, and its close stamps `lastActivityAt`, so idle time is measured from the disconnect. Port sessions carry no byte limit on either transport: nothing is buffered or stored beyond the stream window, and `maximumSessionSeconds` bounds their duration. Port traffic never changes the Sandbox resource `revision`, which changes only when the Sandbox itself does.

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

The guest-facing half is identical for both transports. A dedicated guest-protocol stream dials only the approved `127.0.0.1:<guest-port>` TCP endpoint inside the guest; neither the Runner nor guest agent creates a wildcard listener, host publication, DNAT rule, or fallback route. On that stream the Runner holds a fixed receive window per Port: credit granted to the guest plus received bytes not yet forwarded never exceed it, and credit returns to the guest only as the Runner forwards bytes. A consumer slower than the guest application therefore slows the guest socket; the Runner neither buffers without bound nor closes a healthy connection for being fast. The guest TAP, bridge, and per-assignment nftables table are not in the Port path, so neither transport changes network policy. TCP and HTTP policies currently use the same binary TCP tunnel.

The public API never returns a bridge address, TAP address, or raw host port, and returns a Runner data-plane address only to an authority holding the exact direct-endpoint grant.

Useful activity starts only when the credential is consumed, not when the session is created or inspected. Client disconnect, terminal delivery, expiry, Lease or generation fencing, operator drain, Instance termination, and Runner disconnect close activity and the session deterministically on both transports; a direct connection's live sockets are closed by the same events, and the Runner returns bounded proof of closure. Without an admitted session, unsolicited inbound traffic toward every TAP remains denied.

UDP, port ranges, public unauthenticated sharing, and ungranted direct access are unsupported. UDP and port ranges require kernel-path forwarding and a flow-lifetime model with no analogue in the current connection-scoped session semantics.

## Evidence

Network decisions emit fixed-shape audit evidence containing request, Sandbox, generation, policy revision, normalized destination class, decision, and reason. Logs do not record credentials, full request bodies, DNS payload contents beyond bounded destination evidence, or workspace data.

Both transports retain the same payload-free session accounting and fixed-shape Runner evidence at admitted open and close. Neither transport persists Port payloads or promises payload reconstruction, and no evidence record can contain a payload byte, a credential, a fencing token, or a Runner address.

See [Profiles and authorization](profiles-and-authorization.md), [API conventions](api-conventions.md), [Security](security.md), and [Recovery and reconciliation](recovery-and-reconciliation.md).
