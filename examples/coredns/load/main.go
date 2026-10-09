// Sends DNS queries to a server at a fixed rate and prints per-second latency.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"
)

func main() {
	server := flag.String("server", "127.0.0.1:1053", "")
	qps := flag.Int("qps", 50, "")
	dur := flag.Duration("for", 12*time.Second, "")
	flag.Parse()
	type res struct {
		at, took time.Duration
		ok       bool
	}
	var mu sync.Mutex
	var all []res
	var wg sync.WaitGroup
	start := time.Now()
	tick := time.NewTicker(time.Second / time.Duration(*qps))
	for i := 0; time.Since(start) < *dur; i++ {
		<-tick.C
		wg.Add(1)
		go func(id uint16) {
			defer wg.Done()
			at := time.Since(start)
			q := []byte{0, 0, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0, 7, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 3, 'c', 'o', 'm', 0, 0, 1, 0, 1}
			binary.BigEndian.PutUint16(q, id)
			c, err := net.Dial("udp", *server)
			if err != nil {
				panic(err)
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(6 * time.Second))
			t0 := time.Now()
			c.Write(q)
			b := make([]byte, 1500)
			n, err := c.Read(b)
			ok := err == nil && n >= 12 && binary.BigEndian.Uint16(b) == id && b[3]&0xf == 0
			mu.Lock()
			all = append(all, res{at, time.Since(t0), ok})
			mu.Unlock()
		}(uint16(i))
	}
	wg.Wait()
	sort.Slice(all, func(i, j int) bool { return all[i].at < all[j].at })
	fmt.Println("second  queries  failed  over500ms  max")
	for s := 0; s < int(dur.Seconds()); s++ {
		var n, fail, slow int
		var max time.Duration
		for _, r := range all {
			if int(r.at.Seconds()) != s {
				continue
			}
			n++
			if !r.ok {
				fail++
			}
			if r.took > 500*time.Millisecond {
				slow++
			}
			if r.took > max {
				max = r.took
			}
		}
		fmt.Printf("%6d  %7d  %6d  %9d  %v\n", s, n, fail, slow, max.Round(time.Millisecond))
	}
}
