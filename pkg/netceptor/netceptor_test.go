package netceptor

import (
	"context"
	"fmt"
	"github.com/prep/socketpair"
	"github.com/project-receptor/receptor/pkg/logger"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

type logWriter struct {
	t          *testing.T
	node1count int
	node2count int
}

func (lw *logWriter) Write(p []byte) (n int, err error) {
	s := strings.Trim(string(p), "\n")
	if strings.HasPrefix(s, "ERROR") {
		if !strings.Contains(s, "maximum number of forwarding hops") {
			fmt.Printf(s)
			lw.t.Fatal(s)
			return
		}
	} else if strings.HasPrefix(s, "TRACE") {
		if strings.Contains(s, "via node1") {
			lw.node1count++
		} else if strings.Contains(s, "via node2") {
			lw.node2count++
		}
	}
	lw.t.Log(s)
	return len(p), nil
}

func TestHopCountLimit(t *testing.T) {
	lw := &logWriter{
		t: t,
	}
	log.SetOutput(lw)
	logger.SetShowTrace(true)

	// Create two Netceptor nodes using external backends
	n1 := New(context.Background(), "node1", nil)
	b1, err := NewExternalBackend()
	if err != nil {
		t.Fatal(err)
	}
	err = n1.AddBackend(b1, 1.0, nil)
	if err != nil {
		t.Fatal(err)
	}
	n2 := New(context.Background(), "node2", nil)
	b2, err := NewExternalBackend()
	if err != nil {
		t.Fatal(err)
	}
	err = n2.AddBackend(b2, 1.0, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Create a Unix socket pair and use it to connect the backends
	c1, c2, err := socketpair.New("unix")
	if err != nil {
		t.Fatal(err)
	}

	// Subscribe for node list updates
	nCh1 := n1.SubscribeRoutingUpdates()
	nCh2 := n2.SubscribeRoutingUpdates()

	// Connect the two nodes
	b1.NewConnection(MessageConnFromNetConn(c1), true)
	b2.NewConnection(MessageConnFromNetConn(c2), true)

	// Wait for the nodes to establish routing to each other
	var routes1 map[string]string
	var routes2 map[string]string
	timeout, _ := context.WithTimeout(context.Background(), 5*time.Second)
	for {
		select {
		case <-timeout.Done():
			t.Fatal("timed out waiting for nodes to connect")
		case routes1 = <-nCh1:
		case routes2 = <-nCh2:
		}
		if routes1 != nil && routes2 != nil {
			_, ok := routes1["node2"]
			if ok {
				_, ok := routes2["node1"]
				if ok {
					break
				}
			}
		}
	}

	// Inject a fake node3 that both nodes think the other node has a route to
	n1.addNameHash("node3")
	n1.routingTableLock.Lock()
	n1.routingTable["node3"] = "node2"
	n1.routingTableLock.Unlock()
	n2.addNameHash("node3")
	n2.routingTableLock.Lock()
	n2.routingTable["node3"] = "node1"
	n2.routingTableLock.Unlock()

	// Send a message to node3, which should bounce back and forth until max hops is reached
	pc, err := n1.ListenPacket("test")
	if err != nil {
		t.Fatal(err)
	}
	_, err = pc.WriteTo([]byte("hello"), n1.NewAddr("node3", "test"))
	if err != nil {
		t.Fatal(err)
	}

	// If the hop count limit is not working, the connections will never become inactive
	timeout, _ = context.WithTimeout(context.Background(), 2*time.Second)
	for {
		c, ok := n1.connections["node2"]
		if !ok {
			t.Fatal("node2 disappeared from node1's connections")
		}
		if time.Since(c.lastReceivedData) > 250*time.Millisecond {
			break
		}
		select {
		case <-timeout.Done():
			t.Fatal(timeout.Err())
		case <-time.After(125 * time.Millisecond):
		}
	}

	// Make sure we actually succeeded in creating a routing loop
	if lw.node1count < 10 || lw.node2count < 10 {
		t.Fatal("test did not create a routing loop")
	}

	n1.Shutdown()
	n2.Shutdown()
	n1.BackendWait()
	n2.BackendWait()
}

func TestLotsOfPings(t *testing.T) {
	numBackboneNodes := 3
	numLeafNodesPerBackbone := 3

	nodes := make([]*Netceptor, numBackboneNodes)
	for i := 0; i < numBackboneNodes; i++ {
		nodes[i] = New(context.Background(), fmt.Sprintf("backbone_%d", i), nil)
	}
	for i := 0; i < numBackboneNodes; i++ {
		for j := 0; j < i; j++ {
			b1, err := NewExternalBackend()
			if err == nil {
				err = nodes[i].AddBackend(b1, 1.0, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			b2, err := NewExternalBackend()
			if err == nil {
				err = nodes[j].AddBackend(b2, 1.0, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			c1, c2, err := socketpair.New("unix")
			if err != nil {
				t.Fatal(err)
			}
			b1.NewConnection(MessageConnFromNetConn(c1), true)
			b2.NewConnection(MessageConnFromNetConn(c2), true)
		}
	}

	for i := 0; i < numBackboneNodes; i++ {
		for j := 0; j < numLeafNodesPerBackbone; j++ {
			b1, err := NewExternalBackend()
			if err == nil {
				err = nodes[i].AddBackend(b1, 1.0, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			newNode := New(context.Background(), fmt.Sprintf("leaf_%d_%d", i, j), nil)
			nodes = append(nodes, newNode)
			b2, err := NewExternalBackend()
			if err == nil {
				err = newNode.AddBackend(b2, 1.0, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			c1, c2, err := socketpair.New("unix")
			if err != nil {
				t.Fatal(err)
			}
			b1.NewConnection(MessageConnFromNetConn(c1), true)
			b2.NewConnection(MessageConnFromNetConn(c2), true)
		}
	}

	responses := make([][]bool, len(nodes))
	for i := range nodes {
		responses[i] = make([]bool, len(nodes))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	wg := sync.WaitGroup{}
	for i := range nodes {
		for j := range nodes {
			wg.Add(2)
			go func(sender *Netceptor, recipient *Netceptor, response *bool) {
				pc, err := sender.ListenPacket("")
				if err != nil {
					t.Fatal(err)
				}
				go func() {
					defer wg.Done()
					for {
						buf := make([]byte, 1024)
						err := pc.SetReadDeadline(time.Now().Add(1 * time.Second))
						if err != nil {
							t.Fatalf("error in SetReadDeadline: %s", err)
						}
						_, addr, err := pc.ReadFrom(buf)
						if ctx.Err() != nil {
							return
						}
						if err != nil {
							continue
						}
						ncAddr, ok := addr.(Addr)
						if !ok {
							t.Fatal("addr was not a Netceptor address")
						}
						if ncAddr.node != recipient.nodeID {
							t.Fatal("Received response from wrong node")
						}
						*response = true
					}
				}()
				go func() {
					defer wg.Done()
					buf := []byte("test")
					rAddr := sender.NewAddr(recipient.nodeID, "ping")
					for {
						_, _ = pc.WriteTo(buf, rAddr)
						select {
						case <-ctx.Done():
							return
						case <-time.After(100 * time.Millisecond):
						}
						if *response {
							return
						}
					}
				}()
			}(nodes[i], nodes[j], &responses[i][j])
		}
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			good := true
			for i := range nodes {
				for j := range nodes {
					if !responses[i][j] {
						good = false
						break
					}
				}
				if !good {
					break
				}
			}
			if good {
				t.Log("all pings received")
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}()

	t.Log("waiting for done")
	select {
	case <-ctx.Done():
	}
	t.Log("waiting for waitgroup")
	wg.Wait()

	t.Log("shutting down")
	for i := range nodes {
		go nodes[i].Shutdown()
	}
	t.Log("waiting for backends")
	for i := range nodes {
		nodes[i].BackendWait()
	}
}