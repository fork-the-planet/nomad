// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package client

import (
	"fmt"
	"net"
	"net/rpc"
	"testing"
	"time"

	"github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/client/fingerprint"
	"github.com/hashicorp/nomad/client/servers"
	"github.com/hashicorp/nomad/client/serviceregistration/mock"
	"github.com/hashicorp/nomad/client/state"
	agentconsul "github.com/hashicorp/nomad/command/agent/consul"
	"github.com/hashicorp/nomad/helper/pluginutils/catalog"
	"github.com/hashicorp/nomad/helper/pluginutils/singleton"
	"github.com/hashicorp/nomad/helper/pool"
	"github.com/hashicorp/nomad/helper/testlog"
	"github.com/hashicorp/yamux"
	"github.com/shoenig/test/must"
)

// TestClient creates an in-memory client for testing purposes and returns a
// cleanup func to shutdown the client and remove the alloc and state dirs.
//
// There is no need to override the AllocDir or StateDir as they are randomized
// and removed in the returned cleanup function. If they are overridden in the
// callback then the caller still must run the returned cleanup func.
func TestClient(t testing.TB, cb func(c *config.Config)) (*Client, func() error) {
	return TestClientWithRPCs(t, cb, nil)
}

func TestClientWithRPCs(t testing.TB, cb func(c *config.Config), rpcs map[string]interface{}) (*Client, func() error) {
	conf, cleanup := config.TestClientConfig(t)

	// Tighten the fingerprinter timeouts (must be done in client package
	// to avoid circular dependencies)
	if conf.Options == nil {
		conf.Options = make(map[string]string)
	}
	conf.Options[fingerprint.TightenNetworkTimeoutsConfig] = "true"

	logger := testlog.HCLogger(t)
	conf.Logger = logger

	if cb != nil {
		cb(conf)
	}

	// Set the plugin loaders
	if conf.PluginLoader == nil {
		conf.PluginLoader = catalog.TestPluginLoaderWithOptions(t, "", conf.Options, nil)
	}
	if conf.PluginSingletonLoader == nil {
		conf.PluginSingletonLoader = singleton.NewSingletonLoader(logger, conf.PluginLoader)
	}
	mockCatalog := agentconsul.NewMockCatalog(logger)
	mockService := mock.NewServiceRegistrationHandler(logger)
	client, err := NewClient(conf, mockCatalog, nil, mockService, rpcs)
	if err != nil {
		cleanup()
		t.Fatalf("err: %v", err)
	}
	return client, func() error {
		ch := make(chan error)

		go func() {
			defer close(ch)

			// Shutdown client
			err := client.Shutdown()
			if err != nil {
				ch <- fmt.Errorf("failed to shutdown client: %v", err)
			}

			// Call TestClientConfig cleanup
			cleanup()
		}()

		select {
		case e := <-ch:
			return e
		case <-time.After(1 * time.Minute):
			return fmt.Errorf("timed out while shutting down client")
		}
	}
}

// TestRPCOnlyClient is a client that only pings to establish a connection
// with the server and then returns mock RPC responses for those interfaces
// passed in the `rpcs` parameter. Useful for testing client RPCs from the
// server. Returns the Client, a shutdown function, and any error.
func TestRPCOnlyClient(t testing.TB, cb func(c *config.Config), srvAddr net.Addr, rpcs map[string]any) (*Client, func()) {
	t.Helper()
	conf, cleanup := config.TestClientConfig(t)
	conf.StateDBFactory = state.GetStateDBFactory(true)
	if cb != nil {
		cb(conf)
	}

	testLogger := testlog.HCLogger(t)

	client := &Client{
		config:           conf,
		logger:           testLogger,
		shutdownCh:       make(chan struct{}),
		EnterpriseClient: newEnterpriseClient(testLogger),
	}

	client.servers = servers.New(client.logger, client.shutdownCh, client)
	client.registeredCh = make(chan struct{})
	client.rpcServer = rpc.NewServer()
	for name, rpc := range rpcs {
		client.rpcServer.RegisterName(name, rpc)
	}
	client.heartbeatStop = newHeartbeatStop(
		client.getAllocRunner, time.Second, client.logger, client.shutdownCh)
	client.connPool = pool.NewPool(testlog.HCLogger(t), 10*time.Second, 10, nil, yamux.DefaultConfig())
	client.init()

	cancelFunc := func() {
		ch := make(chan error)

		go func() {
			defer close(ch)
			client.connPool.Shutdown()
			close(client.shutdownCh)
			client.shutdownGroup.Wait()
			cleanup()
		}()

		select {
		case <-ch:
			return
		case <-time.After(5 * time.Second):
			t.Error("timed out while shutting down client")
			return
		}
	}

	go client.rpcConnListener()

	_, err := client.SetServers([]string{srvAddr.String()})
	must.NoError(t, err, must.Sprintf("could not set servers: %v", err))

	client.shutdownGroup.Go(client.registerAndHeartbeat)

	return client, cancelFunc
}
