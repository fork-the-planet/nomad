// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package pool

import (
	"fmt"
	"math"
	"net"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/testlog"
	"github.com/hashicorp/yamux"
	"github.com/shoenig/test/must"
)

func newTestPool(t *testing.T) *ConnPool {
	l := testlog.HCLogger(t)
	p := NewPool(l, 1*time.Minute, 10, nil, yamux.DefaultConfig())
	return p
}

func Test_NewPool(t *testing.T) {

	// Generate a custom yamux configuration, so we can ensure this gets stored
	// as expected.
	yamuxConfig := yamux.DefaultConfig()
	yamuxConfig.AcceptBacklog = math.MaxInt

	testPool := NewPool(hclog.NewNullLogger(), 10*time.Second, 10, nil, yamuxConfig)
	must.NotNil(t, testPool)
	must.NotNil(t, testPool.yamuxCfg)
	must.Eq(t, yamuxConfig.AcceptBacklog, testPool.yamuxCfg.AcceptBacklog)
}

func TestConnPool_ConnListener(t *testing.T) {
	ports := ci.PortAllocator.Grab(1)
	addrStr := fmt.Sprintf("127.0.0.1:%d", ports[0])
	addr, err := net.ResolveTCPAddr("tcp", addrStr)
	must.NoError(t, err)

	exitCh := make(chan struct{})
	defer close(exitCh)
	go func() {
		ln, listenErr := net.Listen("tcp", addrStr)
		must.NoError(t, listenErr)
		defer func() { _ = ln.Close() }()
		conn, _ := ln.Accept()
		defer func() { _ = conn.Close() }()
		<-exitCh
	}()

	time.Sleep(100 * time.Millisecond)

	// Create a test pool
	pool := newTestPool(t)

	// Setup a listener
	c := make(chan *Conn, 1)
	pool.SetConnListener(c)

	// Make an RPC
	_, err = pool.acquire("test", addr)
	must.NoError(t, err)

	// Assert we get a connection.
	select {
	case <-c:
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("timeout")
	}

	// Test that the channel is closed when the pool shuts down.
	err = pool.Shutdown()
	must.NoError(t, err)

	_, ok := <-c
	must.False(t, ok)
}
