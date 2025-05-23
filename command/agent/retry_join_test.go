// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package agent

import (
	"context"
	"fmt"
	golog "log"
	"net"
	"os"
	"testing"
	"time"

	"github.com/hashicorp/cli"
	"github.com/hashicorp/go-netaddrs"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper"
	"github.com/hashicorp/nomad/helper/testlog"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
)

const stubAddress = "127.0.0.1"

type MockDiscover struct {
	ReceivedConfig string
}

func (m *MockDiscover) Addrs(s string, l *golog.Logger) ([]string, error) {
	m.ReceivedConfig = s
	return []string{stubAddress}, nil
}
func (m *MockDiscover) Help() string { return "" }
func (m *MockDiscover) Names() []string {
	return []string{""}
}

type MockNetaddrs struct {
	ReceivedConfig []string
}

func (m *MockNetaddrs) IPAddrs(ctx context.Context, cfg string, l netaddrs.Logger) ([]net.IPAddr, error) {
	m.ReceivedConfig = append(m.ReceivedConfig, cfg)

	ip := net.ParseIP(stubAddress)
	if ip == nil {
		return nil, fmt.Errorf("unable to transform the stubAddress into a valid IP")
	}

	return []net.IPAddr{{IP: ip}}, nil
}

func TestRetryJoin_Integration(t *testing.T) {
	ci.Parallel(t)

	// Create two agents and have one retry join the other
	agent := NewTestAgent(t, t.Name(), nil)
	defer agent.Shutdown()

	agent2 := NewTestAgent(t, t.Name(), func(c *Config) {
		c.NodeName = "foo"
		if c.Server.ServerJoin == nil {
			c.Server.ServerJoin = &ServerJoin{}
		}
		c.Server.ServerJoin.RetryJoin = []string{agent.Config.normalizedAddrs.Serf}
		c.Server.ServerJoin.RetryInterval = 1 * time.Second
	})
	defer agent2.Shutdown()

	// Create a fake command and have it wrap the second agent and run the retry
	// join handler
	cmd := &Command{
		Ui: &cli.BasicUi{
			Reader:      os.Stdin,
			Writer:      os.Stdout,
			ErrorWriter: os.Stderr,
		},
		agent: agent2.Agent,
	}

	if err := cmd.handleRetryJoin(agent2.Config); err != nil {
		t.Fatalf("handleRetryJoin failed: %v", err)
	}

	// Ensure the retry join occurred.
	testutil.WaitForResult(func() (bool, error) {
		mem := agent.server.Members()
		if len(mem) != 2 {
			return false, fmt.Errorf("bad :%#v", mem)
		}
		return true, nil
	}, func(err error) {
		t.Fatal(err)
	})
}

func TestRetryJoin_Server_NonCloud(t *testing.T) {
	ci.Parallel(t)

	var output []string

	mockJoin := func(s []string) (int, error) {
		output = s
		return 0, nil
	}

	joiner := retryJoiner{
		autoDiscover: autoDiscover{goDiscover: &MockDiscover{}},
		joinCfg: &ServerJoin{
			RetryMaxAttempts: 1,
			RetryJoin:        []string{"127.0.0.1"},
		},
		joinFunc: mockJoin,
		logger:   testlog.HCLogger(t),
		errCh:    make(chan struct{}),
	}

	joiner.RetryJoin()

	must.Eq(t, 1, len(output))
	must.Eq(t, stubAddress, output[0])
}

func TestRetryJoin_Server_Cloud(t *testing.T) {
	ci.Parallel(t)

	var output []string

	mockJoin := func(s []string) (int, error) {
		output = s
		return 0, nil
	}

	mockDiscover := &MockDiscover{}
	joiner := retryJoiner{
		autoDiscover: autoDiscover{goDiscover: mockDiscover},
		joinCfg: &ServerJoin{
			RetryMaxAttempts: 1,
			RetryJoin:        []string{"provider=aws, tag_value=foo"},
		},
		joinFunc: mockJoin,
		logger:   testlog.HCLogger(t),
		errCh:    make(chan struct{}),
	}

	joiner.RetryJoin()

	must.Eq(t, 1, len(output))
	must.Eq(t, "provider=aws, tag_value=foo", mockDiscover.ReceivedConfig)
	must.Eq(t, stubAddress, output[0])
}

func TestRetryJoin_Server_MixedProvider(t *testing.T) {
	ci.Parallel(t)

	var output []string

	mockJoin := func(s []string) (int, error) {
		output = s
		return 0, nil
	}

	mockDiscover := &MockDiscover{}
	joiner := retryJoiner{
		autoDiscover: autoDiscover{goDiscover: mockDiscover},
		joinCfg: &ServerJoin{
			RetryMaxAttempts: 1,
			RetryJoin:        []string{"provider=aws, tag_value=foo", "127.0.0.1"},
		},
		joinFunc: mockJoin,
		logger:   testlog.HCLogger(t),
		errCh:    make(chan struct{}),
	}

	joiner.RetryJoin()

	must.Eq(t, 2, len(output))
	must.Eq(t, "provider=aws, tag_value=foo", mockDiscover.ReceivedConfig)
	must.Eq(t, stubAddress, output[0])
}

func TestRetryJoin_AutoDiscover(t *testing.T) {
	ci.Parallel(t)

	var joinAddrs []string
	mockJoin := func(s []string) (int, error) {
		joinAddrs = s
		return 0, nil
	}

	mockDiscover := &MockDiscover{}
	mockNetaddrs := &MockNetaddrs{}

	// 'exec=*'' tests autoDiscover go-netaddr support
	// 'provider=aws, tag_value=foo' ensures that provider-prefixed configs are routed to go-discover
	// 'localhost' ensures that bare hostnames are returned as-is
	// 'localhost2:4648' ensures hostname:port entries are returned as-is
	// '127.0.0.1:4648' ensures ip:port entiresare returned as-is
	// '100.100.100.100' ensures that bare IPs are returned as-is
	joiner := retryJoiner{
		autoDiscover: autoDiscover{goDiscover: mockDiscover, netAddrs: mockNetaddrs},
		joinCfg: &ServerJoin{
			RetryMaxAttempts: 1,
			RetryJoin: []string{
				"exec=echo 127.0.0.1", "provider=aws, tag_value=foo",
				"localhost", "localhost2:4648", "127.0.0.1:4648", "100.100.100.100"},
		},
		joinFunc: mockJoin,
		logger:   testlog.HCLogger(t),
		errCh:    make(chan struct{}),
	}

	joiner.RetryJoin()

	must.Eq(t, []string{
		"127.0.0.1", "127.0.0.1", "localhost", "localhost2:4648",
		"127.0.0.1:4648", "100.100.100.100"},
		joinAddrs)
	must.Eq(t, []string{"exec=echo 127.0.0.1"}, mockNetaddrs.ReceivedConfig)
	must.Eq(t, "provider=aws, tag_value=foo", mockDiscover.ReceivedConfig)
}

func TestRetryJoin_Client(t *testing.T) {
	ci.Parallel(t)

	var output []string

	mockJoin := func(s []string) (int, error) {
		output = s
		return 0, nil
	}

	joiner := retryJoiner{
		autoDiscover: autoDiscover{goDiscover: &MockDiscover{}},
		joinCfg: &ServerJoin{
			RetryMaxAttempts: 1,
			RetryJoin:        []string{"127.0.0.1"},
		},
		joinFunc: mockJoin,
		logger:   testlog.HCLogger(t),
		errCh:    make(chan struct{}),
	}

	joiner.RetryJoin()

	must.Eq(t, 1, len(output))
	must.Eq(t, stubAddress, output[0])
}

// MockFailDiscover implements the DiscoverInterface interface and can be used
// for tests that want to purposely fail the discovery process.
type MockFailDiscover struct {
	ReceivedConfig string
}

func (m *MockFailDiscover) Addrs(cfg string, _ *golog.Logger) ([]string, error) {
	return nil, fmt.Errorf("test: failed discovery %q", cfg)
}
func (m *MockFailDiscover) Help() string { return "" }
func (m *MockFailDiscover) Names() []string {
	return []string{""}
}

func TestRetryJoin_RetryMaxAttempts(t *testing.T) {
	ci.Parallel(t)

	// Create an error channel to pass to the retry joiner. When the retry
	// attempts have been exhausted, this channel is closed and our only way
	// to test this apart from inspecting log entries.
	errCh := make(chan struct{})

	// Create a timeout to protect against problems within the test blocking
	// for arbitrary long times.
	timeout, timeoutStop := helper.NewSafeTimer(2 * time.Second)
	defer timeoutStop()

	var output []string

	joiner := retryJoiner{
		autoDiscover: autoDiscover{goDiscover: &MockFailDiscover{}},
		joinCfg:      &ServerJoin{RetryMaxAttempts: 1, RetryJoin: []string{"provider=foo"}},
		joinFunc: func(s []string) (int, error) {
			output = s
			return 0, nil
		},
		logger: testlog.HCLogger(t),
		errCh:  errCh,
	}

	// Execute the retry join function in a routine, so we can track whether
	// this returns and exits without close the error channel and thus
	// indicating retry failure.
	doneCh := make(chan struct{})

	go func(doneCh chan struct{}) {
		joiner.RetryJoin()
		close(doneCh)
	}(doneCh)

	// The main test; ensure error channel is closed, indicating the retry
	// limit has been reached.
	select {
	case <-errCh:
		must.Len(t, 0, output)
	case <-doneCh:
		t.Fatal("retry join completed without closing error channel")
	case <-timeout.C:
		t.Fatal("timeout reached without error channel close")
	}
}

func TestRetryJoin_Validate(t *testing.T) {
	ci.Parallel(t)
	type validateExpect struct {
		config  *Config
		isValid bool
		reason  string
	}

	scenarios := []*validateExpect{
		{
			config: &Config{
				Server: &ServerConfig{
					ServerJoin: &ServerJoin{
						RetryJoin:        []string{"127.0.0.1"},
						RetryMaxAttempts: 0,
						RetryInterval:    0,
						StartJoin:        []string{},
					},
					RetryJoin:        []string{"127.0.0.1"},
					RetryMaxAttempts: 0,
					RetryInterval:    0,
					StartJoin:        []string{},
				},
			},
			isValid: false,
			reason:  "server_join cannot be defined if retry_join is defined on the server block",
		},
		{
			config: &Config{
				Server: &ServerConfig{
					ServerJoin: &ServerJoin{
						RetryJoin:        []string{"127.0.0.1"},
						RetryMaxAttempts: 0,
						RetryInterval:    0,
						StartJoin:        []string{},
					},
					StartJoin:        []string{"127.0.0.1"},
					RetryMaxAttempts: 0,
					RetryInterval:    0,
					RetryJoin:        []string{},
				},
			},
			isValid: false,
			reason:  "server_join cannot be defined if start_join is defined on the server block",
		},
		{
			config: &Config{
				Server: &ServerConfig{
					ServerJoin: &ServerJoin{
						RetryJoin:        []string{"127.0.0.1"},
						RetryMaxAttempts: 0,
						RetryInterval:    0,
						StartJoin:        []string{},
					},
					StartJoin:        []string{},
					RetryMaxAttempts: 1,
					RetryInterval:    0,
					RetryJoin:        []string{},
				},
			},
			isValid: false,
			reason:  "server_join cannot be defined if retry_max_attempts is defined on the server block",
		},
		{
			config: &Config{
				Server: &ServerConfig{
					ServerJoin: &ServerJoin{
						RetryJoin:        []string{"127.0.0.1"},
						RetryMaxAttempts: 0,
						RetryInterval:    time.Duration(1),
						StartJoin:        []string{},
					},
					StartJoin:        []string{},
					RetryMaxAttempts: 0,
					RetryInterval:    3 * time.Second,
					RetryJoin:        []string{},
				},
			},
			isValid: false,
			reason:  "server_join cannot be defined if retry_interval is defined on the server block",
		},
		{
			config: &Config{
				Server: &ServerConfig{
					ServerJoin: &ServerJoin{
						RetryJoin:        []string{"127.0.0.1"},
						RetryMaxAttempts: 0,
						RetryInterval:    0,
						StartJoin:        []string{"127.0.0.1"},
					},
				},
			},
			isValid: false,
			reason:  "start_join and retry_join should not both be defined",
		},
		{
			config: &Config{
				Client: &ClientConfig{
					ServerJoin: &ServerJoin{
						RetryJoin:        []string{},
						RetryMaxAttempts: 0,
						RetryInterval:    0,
						StartJoin:        []string{"127.0.0.1"},
					},
				},
			},
			isValid: false,
			reason:  "start_join should not be defined on the client",
		},
		{
			config: &Config{
				Client: &ClientConfig{
					ServerJoin: &ServerJoin{
						RetryJoin:        []string{"127.0.0.1"},
						RetryMaxAttempts: 0,
						RetryInterval:    0,
					},
				},
			},
			isValid: true,
			reason:  "client server_join should be valid",
		},
		{
			config: &Config{
				Server: &ServerConfig{
					ServerJoin: &ServerJoin{
						RetryJoin:        []string{"127.0.0.1"},
						RetryMaxAttempts: 1,
						RetryInterval:    1,
						StartJoin:        []string{},
					},
				},
			},
			isValid: true,
			reason:  "server server_join should be valid",
		},
	}

	joiner := retryJoiner{}
	for _, scenario := range scenarios {
		t.Run(scenario.reason, func(t *testing.T) {
			err := joiner.Validate(scenario.config)
			if scenario.isValid {
				must.NoError(t, err)
			} else {
				must.Error(t, err)
			}
		})
	}
}
