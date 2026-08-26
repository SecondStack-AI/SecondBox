package firecracker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

const (
	runnerDNSMaximumMessageBytes = 4096
	runnerDNSMaximumConcurrent   = 64
	runnerDNSMaximumCNAMEDepth   = 8
	runnerDNSDeadline            = 5 * time.Second
)

type runnerDNSProxy struct {
	listen    netip.Addr
	upstream  netip.AddrPort
	observe   func(context.Context, string, string, []netip.Addr, time.Duration) error
	onFailure func(error)

	listenUDP func(string, *net.UDPAddr) (*net.UDPConn, error)
	listenTCP func(string, string) (net.Listener, error)

	mu        sync.Mutex
	started   bool
	closing   bool
	healthErr error
	udp       *net.UDPConn
	tcp       net.Listener
	workers   chan struct{}
}

type dnsValidatedQuestion struct {
	header   dnsmessage.Header
	question dnsmessage.Question
}

func (p *runnerDNSProxy) Start() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started {
		return p.healthErr
	}
	if !p.listen.IsValid() || !p.upstream.IsValid() || p.observe == nil {
		return fmt.Errorf("runner DNS proxy settings are incomplete")
	}
	listenUDP := p.listenUDP
	if listenUDP == nil {
		listenUDP = net.ListenUDP
	}
	listenTCP := p.listenTCP
	if listenTCP == nil {
		listenTCP = net.Listen
	}
	address := netip.AddrPortFrom(p.listen, 53)
	udp, err := listenUDP("udp", net.UDPAddrFromAddrPort(address))
	if err != nil {
		return fmt.Errorf("bind runner DNS UDP %s: %w", address, err)
	}
	tcp, err := listenTCP("tcp", address.String())
	if err != nil {
		return errors.Join(
			fmt.Errorf("bind runner DNS TCP %s: %w", address, err),
			udp.Close(),
		)
	}
	p.udp = udp
	p.tcp = tcp
	p.workers = make(chan struct{}, runnerDNSMaximumConcurrent)
	p.started = true
	go p.serveUDP(udp)
	go p.serveTCP(tcp)
	return nil
}

func (p *runnerDNSProxy) Health() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.started {
		return fmt.Errorf("runner DNS proxy is not started")
	}
	return p.healthErr
}

func (p *runnerDNSProxy) Close() error {
	p.mu.Lock()
	if !p.started {
		p.mu.Unlock()
		return nil
	}
	p.closing = true
	udp, tcp := p.udp, p.tcp
	p.mu.Unlock()
	var closeErr error
	if udp != nil {
		closeErr = errors.Join(closeErr, udp.Close())
	}
	if tcp != nil {
		closeErr = errors.Join(closeErr, tcp.Close())
	}
	p.mu.Lock()
	p.started = false
	p.udp = nil
	p.tcp = nil
	p.mu.Unlock()
	return closeErr
}

func (p *runnerDNSProxy) fail(err error) {
	if err == nil {
		return
	}
	p.mu.Lock()
	if p.closing || p.healthErr != nil {
		p.mu.Unlock()
		return
	}
	p.healthErr = err
	callback := p.onFailure
	p.mu.Unlock()
	if callback != nil {
		callback(err)
	}
}

func (p *runnerDNSProxy) acquireWorker() bool {
	select {
	case p.workers <- struct{}{}:
		return true
	default:
		return false
	}
}

func (p *runnerDNSProxy) releaseWorker() {
	<-p.workers
}

func (p *runnerDNSProxy) serveUDP(listener *net.UDPConn) {
	buffer := make([]byte, runnerDNSMaximumMessageBytes+1)
	for {
		count, source, err := listener.ReadFromUDP(buffer)
		if err != nil {
			p.fail(fmt.Errorf("runner DNS UDP listener failed: %w", err))
			return
		}
		query := append([]byte(nil), buffer[:count]...)
		if count > runnerDNSMaximumMessageBytes || !p.acquireWorker() {
			if _, err := listener.WriteToUDP(refusedDNSResponse(query), source); err != nil {
				p.fail(fmt.Errorf("runner DNS UDP refusal failed: %w", err))
				return
			}
			continue
		}
		go func() {
			defer p.releaseWorker()
			response, exchangeErr := p.exchange(context.Background(), source.AddrPort().Addr().String(), query, "udp")
			if exchangeErr != nil {
				response = refusedDNSResponse(query)
			}
			if _, err := listener.WriteToUDP(response, source); err != nil {
				p.fail(fmt.Errorf("runner DNS UDP response failed: %w", err))
			}
		}()
	}
}

func (p *runnerDNSProxy) serveTCP(listener net.Listener) {
	for {
		connection, err := listener.Accept()
		if err != nil {
			p.fail(fmt.Errorf("runner DNS TCP listener failed: %w", err))
			return
		}
		if !p.acquireWorker() {
			if err := connection.Close(); err != nil {
				p.fail(fmt.Errorf("close over-capacity runner DNS TCP connection: %w", err))
			}
			continue
		}
		go func() {
			defer p.releaseWorker()
			if err := p.handleTCP(connection); err != nil {
				// Query framing and client disconnects are per-request failures.
				return
			}
		}()
	}
}

func (p *runnerDNSProxy) handleTCP(connection net.Conn) (resultErr error) {
	defer func() {
		resultErr = errors.Join(resultErr, connection.Close())
	}()
	if err := connection.SetDeadline(time.Now().Add(runnerDNSDeadline)); err != nil {
		return err
	}
	query, err := readDNSFrame(connection)
	if err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(connection.RemoteAddr().String())
	if err != nil {
		return err
	}
	response, err := p.exchange(context.Background(), host, query, "tcp")
	if err != nil {
		response = refusedDNSResponse(query)
	}
	return writeDNSFrame(connection, response)
}

func readDNSFrame(reader io.Reader) ([]byte, error) {
	var length [2]byte
	if _, err := io.ReadFull(reader, length[:]); err != nil {
		return nil, err
	}
	size := int(length[0])<<8 | int(length[1])
	if size < 12 || size > runnerDNSMaximumMessageBytes {
		return nil, fmt.Errorf("DNS TCP message size %d is outside 12..%d", size, runnerDNSMaximumMessageBytes)
	}
	message := make([]byte, size)
	_, err := io.ReadFull(reader, message)
	return message, err
}

func writeDNSFrame(writer io.Writer, message []byte) error {
	if len(message) > runnerDNSMaximumMessageBytes {
		return fmt.Errorf("DNS response exceeds %d bytes", runnerDNSMaximumMessageBytes)
	}
	length := []byte{byte(len(message) >> 8), byte(len(message))}
	if _, err := writer.Write(length); err != nil {
		return err
	}
	_, err := writer.Write(message)
	return err
}

func (p *runnerDNSProxy) exchange(
	ctx context.Context,
	guestIP string,
	query []byte,
	network string,
) (result []byte, resultErr error) {
	validatedQuery, err := validateDNSQuery(query)
	if err != nil {
		return nil, err
	}
	dialer := net.Dialer{Timeout: runnerDNSDeadline}
	connection, err := dialer.DialContext(ctx, network, p.upstream.String())
	if err != nil {
		return nil, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, connection.Close())
	}()
	if err := connection.SetDeadline(time.Now().Add(runnerDNSDeadline)); err != nil {
		return nil, err
	}
	var response []byte
	if network == "tcp" {
		if err := writeDNSFrame(connection, query); err != nil {
			return nil, err
		}
		response, err = readDNSFrame(connection)
	} else {
		if _, err := connection.Write(query); err != nil {
			return nil, err
		}
		buffer := make([]byte, runnerDNSMaximumMessageBytes+1)
		count, readErr := connection.Read(buffer)
		if readErr != nil {
			return nil, readErr
		}
		if count > runnerDNSMaximumMessageBytes {
			return nil, fmt.Errorf("DNS UDP response exceeds %d bytes", runnerDNSMaximumMessageBytes)
		}
		response = append([]byte(nil), buffer[:count]...)
	}
	if err != nil {
		return nil, err
	}
	addresses, ttl, err := validateDNSResponse(validatedQuery, response)
	if err != nil {
		return nil, err
	}
	if len(addresses) == 0 {
		return response, nil
	}
	if err := p.observe(ctx, guestIP, validatedQuery.question.Name.String(), addresses, ttl); err != nil {
		return nil, err
	}
	return response, nil
}

func validateDNSQuery(message []byte) (dnsValidatedQuestion, error) {
	if len(message) < 12 || len(message) > runnerDNSMaximumMessageBytes {
		return dnsValidatedQuestion{}, fmt.Errorf("DNS query size is invalid")
	}
	var parser dnsmessage.Parser
	header, err := parser.Start(message)
	if err != nil || header.Response {
		return dnsValidatedQuestion{}, fmt.Errorf("DNS query header is invalid")
	}
	question, err := parser.Question()
	if err != nil {
		return dnsValidatedQuestion{}, fmt.Errorf("DNS query question: %w", err)
	}
	if question.Class != dnsmessage.ClassINET ||
		(question.Type != dnsmessage.TypeA && question.Type != dnsmessage.TypeAAAA) {
		return dnsValidatedQuestion{}, fmt.Errorf("DNS query must be IN A or AAAA")
	}
	if _, err := parser.Question(); !errors.Is(err, dnsmessage.ErrSectionDone) {
		return dnsValidatedQuestion{}, fmt.Errorf("DNS query must contain exactly one question")
	}
	if _, err := parser.Answer(); !errors.Is(err, dnsmessage.ErrSectionDone) {
		return dnsValidatedQuestion{}, fmt.Errorf("DNS query cannot contain answers")
	}
	if _, err := parser.Authority(); !errors.Is(err, dnsmessage.ErrSectionDone) {
		return dnsValidatedQuestion{}, fmt.Errorf("DNS query cannot contain authority records")
	}
	for {
		resource, err := parser.Additional()
		if errors.Is(err, dnsmessage.ErrSectionDone) {
			break
		}
		if err != nil {
			return dnsValidatedQuestion{}, fmt.Errorf("DNS query additional record: %w", err)
		}
		if resource.Header.Type != dnsmessage.TypeOPT {
			return dnsValidatedQuestion{}, fmt.Errorf("DNS query additional records must be OPT")
		}
	}
	return dnsValidatedQuestion{header: header, question: question}, nil
}

func validateDNSResponse(query dnsValidatedQuestion, message []byte) ([]netip.Addr, time.Duration, error) {
	if len(message) < 12 || len(message) > runnerDNSMaximumMessageBytes {
		return nil, 0, fmt.Errorf("DNS response size is invalid")
	}
	var parser dnsmessage.Parser
	header, err := parser.Start(message)
	if err != nil ||
		!header.Response ||
		header.ID != query.header.ID ||
		(header.RCode != dnsmessage.RCodeSuccess && header.RCode != dnsmessage.RCodeNameError) {
		return nil, 0, fmt.Errorf("DNS response header does not match the query")
	}
	question, err := parser.Question()
	if err != nil || question != query.question {
		return nil, 0, fmt.Errorf("DNS response question does not match the query")
	}
	if _, err := parser.Question(); !errors.Is(err, dnsmessage.ErrSectionDone) {
		return nil, 0, fmt.Errorf("DNS response must echo exactly one question")
	}

	cnames := make(map[dnsmessage.Name]dnsmessage.Name)
	cnameTTLs := make(map[dnsmessage.Name]uint32)
	addressRecords := make(map[dnsmessage.Name][]netip.Addr)
	recordTTLs := make(map[dnsmessage.Name]uint32)
	for {
		resource, err := parser.Answer()
		if errors.Is(err, dnsmessage.ErrSectionDone) {
			break
		}
		if err != nil {
			return nil, 0, fmt.Errorf("parse DNS answer: %w", err)
		}
		if resource.Header.Class != dnsmessage.ClassINET {
			continue
		}
		switch body := resource.Body.(type) {
		case *dnsmessage.CNAMEResource:
			if existing, found := cnames[resource.Header.Name]; found && existing != body.CNAME {
				return nil, 0, fmt.Errorf("DNS owner has conflicting CNAME targets")
			}
			cnames[resource.Header.Name] = body.CNAME
			cnameTTLs[resource.Header.Name] = minimumPositiveTTL(cnameTTLs[resource.Header.Name], resource.Header.TTL)
		case *dnsmessage.AResource:
			if query.question.Type == dnsmessage.TypeA {
				addressRecords[resource.Header.Name] = append(
					addressRecords[resource.Header.Name],
					netip.AddrFrom4(body.A),
				)
				recordTTLs[resource.Header.Name] = minimumPositiveTTL(recordTTLs[resource.Header.Name], resource.Header.TTL)
			}
		case *dnsmessage.AAAAResource:
			if query.question.Type == dnsmessage.TypeAAAA {
				addressRecords[resource.Header.Name] = append(
					addressRecords[resource.Header.Name],
					netip.AddrFrom16(body.AAAA),
				)
				recordTTLs[resource.Header.Name] = minimumPositiveTTL(recordTTLs[resource.Header.Name], resource.Header.TTL)
			}
		}
	}
	// A name-error response is only ever forwarded as the strictly empty
	// negative answer below; one that also carries resolving records is
	// contradictory and stays rejected as injection.
	if header.RCode == dnsmessage.RCodeNameError && (len(addressRecords) != 0 || len(cnames) != 0) {
		return nil, 0, fmt.Errorf("DNS name-error response must carry no answers")
	}
	owner := query.question.Name
	seen := make(map[dnsmessage.Name]bool)
	var chainTTL uint32
	for depth := 0; depth <= runnerDNSMaximumCNAMEDepth; depth++ {
		if seen[owner] {
			return nil, 0, fmt.Errorf("DNS CNAME chain contains a loop")
		}
		seen[owner] = true
		if addresses := addressRecords[owner]; len(addresses) > 0 {
			ttl := minimumPositiveTTL(chainTTL, recordTTLs[owner])
			if ttl == 0 {
				return nil, 0, fmt.Errorf("DNS address TTL must be positive")
			}
			return addresses, time.Duration(ttl) * time.Second, nil
		}
		target, found := cnames[owner]
		if !found {
			// A strictly empty answer (an empty NOERROR or NXDOMAIN, commonly
			// the AAAA half of a dual query) is forwarded without pinning
			// anything: strict resolvers fail the whole lookup when it is
			// refused instead, and forwarding it admits no destination. Any
			// answer carrying records that do not resolve the question stays
			// rejected as injection.
			if len(addressRecords) == 0 && len(cnames) == 0 {
				return nil, 0, nil
			}
			return nil, 0, fmt.Errorf("DNS response has no address on the validated owner chain")
		}
		if cnameTTLs[owner] == 0 {
			return nil, 0, fmt.Errorf("DNS CNAME TTL must be positive")
		}
		chainTTL = minimumPositiveTTL(chainTTL, cnameTTLs[owner])
		owner = target
	}
	return nil, 0, fmt.Errorf("DNS CNAME chain exceeds %d records", runnerDNSMaximumCNAMEDepth)
}

func minimumPositiveTTL(current, candidate uint32) uint32 {
	if candidate == 0 {
		return current
	}
	if current == 0 || candidate < current {
		return candidate
	}
	return current
}

func refusedDNSResponse(query []byte) []byte {
	response := append([]byte(nil), query...)
	if len(response) >= 4 {
		response[2] |= 0x80
		response[3] = (response[3] & 0xf0) | 0x05
	}
	return response
}
