package main

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"sync/atomic"
)

// spreadDialer resolves the target host once and round-robins new connections
// across all of its IPs (for an ALB: one IP per node/AZ). Without it, a burst of
// simultaneous dials shares one in-flight DNS lookup and tends to land every
// connection on the same IP, i.e. on a single ALB node.
func spreadDialer(d *net.Dialer, baseURL string) (func(ctx context.Context, network, addr string) (net.Conn, error), []string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, nil, err
	}
	ips, err := net.DefaultResolver.LookupHost(context.Background(), u.Hostname())
	if err != nil {
		return nil, nil, err
	}
	if len(ips) == 0 {
		return nil, nil, fmt.Errorf("no IPs for %s", u.Hostname())
	}
	var n atomic.Uint64
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ip := ips[n.Add(1)%uint64(len(ips))]
		return d.DialContext(ctx, network, net.JoinHostPort(ip, port))
	}, ips, nil
}
