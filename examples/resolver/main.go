// Resolver looks a name up through Go's own DNS resolver and prints how long the
// lookup took. With -errqueue it enables errqueue on the resolver's UDP sockets
// through Resolver.Dial, so an ICMP error fails the lookup instead of a timeout.
//
//	go run ./examples/resolver -server 10.0.9.1:53 -errqueue
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"time"

	"github.com/MohibShaikh/errqueue"
)

func main() {
	server := flag.String("server", "127.0.0.1:53", "DNS server to send every query to")
	name := flag.String("name", "example.com.", "name to look up")
	enable := flag.Bool("errqueue", false, "enable errqueue on the resolver's UDP sockets")
	flag.Parse()

	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			c, err := d.DialContext(ctx, network, *server)
			if err != nil || !*enable {
				return c, err
			}
			if uc, ok := c.(*net.UDPConn); ok {
				if err := errqueue.Enable(uc); err != nil {
					c.Close()
					return nil, err
				}
			}
			return c, nil
		},
	}
	start := time.Now()
	addrs, err := r.LookupHost(context.Background(), *name)
	took := time.Since(start).Round(time.Millisecond)
	if err != nil {
		fmt.Printf("%v  %v\n", took, err)
		return
	}
	fmt.Printf("%v  %v\n", took, addrs)
}
