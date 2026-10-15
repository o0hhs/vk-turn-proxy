// SPDX-License-Identifier: MIT
//
// Where the client's own sockets are opened. The client has two very different kinds of
// socket: the loopback one the app side talks to (leave it alone), and the ones reaching
// the outside — VK's API and call link, the resolvers, the TURN server itself. A host that
// runs its own tunnel must claim the second kind, or that traffic is routed back into the
// tunnel the TURN client is feeding: a loop, and on Android a silently broken one.

package vkclient

import (
	"context"
	"fmt"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/bschaatsbergen/dnsdialer"
)

// SocketPolicy tells the client how to open its outward sockets. The zero value keeps
// upstream behaviour: plain net.Dialer sockets and the built-in public resolvers.
//
// On Android Control is where VpnService.protect goes — the host passes a function that
// marks every new fd as outside its tunnel.
type SocketPolicy struct {
	// Control runs on the raw socket before connect, exactly like net.Dialer.Control.
	Control func(network, address string, c syscall.RawConn) error
}

var (
	policyMu     sync.RWMutex
	socketPolicy SocketPolicy
)

// SetSocketPolicy installs the policy for clients started afterwards. Call it before Run.
func SetSocketPolicy(policy SocketPolicy) {
	policyMu.Lock()
	socketPolicy = policy
	policyMu.Unlock()
}

func controlFunc() func(network, address string, c syscall.RawConn) error {
	policyMu.RLock()
	defer policyMu.RUnlock()

	return socketPolicy.Control
}

// customResolvers are the resolvers the client resolves VK through: public ones, on purpose.
// A phone behind a hijacked or dead home DNS cannot resolve vk.com with the system resolver,
// and this client's whole point is to work on networks that block it.
var customResolvers = []string{
	"77.88.8.8:53", "77.88.8.1:53", "8.8.8.8:53", "8.8.4.4:53", "1.1.1.1:53", "1.0.0.1:53",
}

func resolver() *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Control: controlFunc()}
			var lastErr error
			for _, dns := range customResolvers {
				conn, err := d.DialContext(ctx, "udp", dns)
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			return nil, lastErr
		},
	}
}

// policyDialer is the outward TCP/UDP dialer: upstream's own resolvers, plus the host's
// socket marking when there is one.
func policyDialer() *net.Dialer {
	return &net.Dialer{
		Timeout:   20 * time.Second,
		KeepAlive: 30 * time.Second,
		Resolver:  resolver(),
		Control:   controlFunc(),
	}
}

func policyDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return policyDialer().DialContext(ctx, network, address)
}

// policyDialUDP opens a connected UDP socket — the shape pion/turn and the DTLS layer want.
func policyDialUDP(addr *net.UDPAddr) (*net.UDPConn, error) {
	conn, err := policyDialContext(context.Background(), "udp", addr.String())
	if err != nil {
		return nil, err
	}

	udpConn, ok := conn.(*net.UDPConn)
	if !ok {
		_ = conn.Close()

		return nil, fmt.Errorf("udp dial to %s returned %T", addr, conn)
	}

	return udpConn, nil
}

func policyListenPacket(network, address string) (net.PacketConn, error) {
	listenConfig := net.ListenConfig{Control: controlFunc()}

	return listenConfig.ListenPacket(context.Background(), network, address)
}

func policyListenUDP(laddr *net.UDPAddr) (*net.UDPConn, error) {
	conn, err := policyListenPacket("udp", laddr.String())
	if err != nil {
		return nil, err
	}

	udpConn, ok := conn.(*net.UDPConn)
	if !ok {
		_ = conn.Close()

		return nil, fmt.Errorf("udp listen on %s returned %T", laddr, conn)
	}

	return udpConn, nil
}

func policyListenTCP(laddr *net.TCPAddr) (*net.TCPListener, error) {
	listenConfig := net.ListenConfig{Control: controlFunc()}

	listener, err := listenConfig.Listen(context.Background(), "tcp", laddr.String())
	if err != nil {
		return nil, err
	}

	tcpListener, ok := listener.(*net.TCPListener)
	if !ok {
		_ = listener.Close()

		return nil, fmt.Errorf("tcp listen on %s returned %T", laddr, listener)
	}

	return tcpListener, nil
}

// vkDialer is everything the VK API, the call link and the captcha page are reached through.
type vkDialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// newVkDialer picks how VK is reached. Without a policy it is upstream's dnsdialer — its own
// resolvers with a cache and a fallback strategy, which is what a desktop CLI wants. With a
// policy it is policyDialer: same resolvers, but the sockets are the host's to mark, because
// inside a VpnService even a hardcoded resolver address would be routed into the tunnel
// while the tunnel itself is waiting for this client.
func newVkDialer() vkDialer {
	if controlFunc() != nil {
		return policyDialer()
	}

	return dnsdialer.New(
		dnsdialer.WithResolvers(customResolvers...),
		dnsdialer.WithStrategy(dnsdialer.Fallback{}),
		dnsdialer.WithCache(100, 10*time.Hour, 10*time.Hour),
	)
}
