// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package fingerprint

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/client/config"
	agentconsul "github.com/hashicorp/nomad/command/agent/consul"
	"github.com/hashicorp/nomad/helper/testlog"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

// fakeConsul creates an HTTP server mimicking Consul /v1/agent/self endpoint on
// the first request, and alternates between success and failure responses on
// subsequent requests
func fakeConsul(payload string) (*httptest.Server, *config.Config) {
	working := true

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if working {
			_, _ = io.WriteString(w, payload)
			working = false
		} else {
			w.WriteHeader(http.StatusInternalServerError)
			working = true
		}
	}))

	cfg := config.DefaultConfig()
	cfg.GetDefaultConsul().Addr = strings.TrimPrefix(ts.URL, `http://`)
	return ts, cfg
}

func fakeConsulPayload(t *testing.T, filename string) string {
	b, err := os.ReadFile(filename)
	must.NoError(t, err)
	return string(b)
}

func newConsulFingerPrint(t *testing.T) *ConsulFingerprint {
	return NewConsulFingerprint(testlog.HCLogger(t)).(*ConsulFingerprint)
}

func TestConsulFingerprint_server(t *testing.T) {
	ci.Parallel(t)

	cfs := new(consulState)

	t.Run("is server", func(t *testing.T) {
		s, ok := cfs.server(agentconsul.Self{
			"Config": {"Server": true},
		})
		must.True(t, ok)
		must.Eq(t, "true", s)
	})

	t.Run("is not server", func(t *testing.T) {
		s, ok := cfs.server(agentconsul.Self{
			"Config": {"Server": false},
		})
		must.True(t, ok)
		must.Eq(t, "false", s)
	})

	t.Run("missing", func(t *testing.T) {
		_, ok := cfs.server(agentconsul.Self{
			"Config": {},
		})
		must.False(t, ok)
	})

	t.Run("malformed", func(t *testing.T) {
		_, ok := cfs.server(agentconsul.Self{
			"Config": {"Server": 9000},
		})
		must.False(t, ok)
	})
}

func TestConsulFingerprint_version(t *testing.T) {
	ci.Parallel(t)

	cfs := new(consulState)

	t.Run("oss", func(t *testing.T) {
		v, ok := cfs.version(agentconsul.Self{
			"Config": {"Version": "v1.9.5"},
		})
		must.True(t, ok)
		must.Eq(t, "v1.9.5", v)
	})

	t.Run("ent", func(t *testing.T) {
		v, ok := cfs.version(agentconsul.Self{
			"Config": {"Version": "v1.9.5+ent"},
		})
		must.True(t, ok)
		must.Eq(t, "v1.9.5+ent", v)
	})

	t.Run("missing", func(t *testing.T) {
		_, ok := cfs.version(agentconsul.Self{
			"Config": {},
		})
		must.False(t, ok)
	})

	t.Run("malformed", func(t *testing.T) {
		_, ok := cfs.version(agentconsul.Self{
			"Config": {"Version": 9000},
		})
		must.False(t, ok)
	})
}

func TestConsulFingerprint_sku(t *testing.T) {
	ci.Parallel(t)

	cfs := new(consulState)

	t.Run("oss", func(t *testing.T) {
		s, ok := cfs.sku(agentconsul.Self{
			"Config": {"Version": "v1.9.5"},
		})
		must.True(t, ok)
		must.Eq(t, "oss", s)
	})

	t.Run("oss dev", func(t *testing.T) {
		s, ok := cfs.sku(agentconsul.Self{
			"Config": {"Version": "v1.9.5-dev"},
		})
		must.True(t, ok)
		must.Eq(t, "oss", s)
	})

	t.Run("ent", func(t *testing.T) {
		s, ok := cfs.sku(agentconsul.Self{
			"Config": {"Version": "v1.9.5+ent"},
		})
		must.True(t, ok)
		must.Eq(t, "ent", s)
	})

	t.Run("ent dev", func(t *testing.T) {
		s, ok := cfs.sku(agentconsul.Self{
			"Config": {"Version": "v1.9.5+ent-dev"},
		})
		must.True(t, ok)
		must.Eq(t, "ent", s)
	})

	t.Run("extra spaces", func(t *testing.T) {
		v, ok := cfs.sku(agentconsul.Self{
			"Config": {"Version": "   v1.9.5\n"},
		})
		must.True(t, ok)
		must.Eq(t, "oss", v)
	})

	t.Run("missing", func(t *testing.T) {
		_, ok := cfs.sku(agentconsul.Self{
			"Config": {},
		})
		must.False(t, ok)
	})

	t.Run("malformed", func(t *testing.T) {
		_, ok := cfs.sku(agentconsul.Self{
			"Config": {"Version": "***"},
		})
		must.False(t, ok)
	})
}

func TestConsulFingerprint_revision(t *testing.T) {
	ci.Parallel(t)

	cfs := new(consulState)

	t.Run("ok", func(t *testing.T) {
		r, ok := cfs.revision(agentconsul.Self{
			"Config": {"Revision": "3c1c22679"},
		})
		must.True(t, ok)
		must.Eq(t, "3c1c22679", r)
	})

	t.Run("malformed", func(t *testing.T) {
		_, ok := cfs.revision(agentconsul.Self{
			"Config": {"Revision": 9000},
		})
		must.False(t, ok)
	})

	t.Run("missing", func(t *testing.T) {
		_, ok := cfs.revision(agentconsul.Self{
			"Config": {},
		})
		must.False(t, ok)
	})
}

func TestConsulFingerprint_dc(t *testing.T) {
	ci.Parallel(t)

	cfs := new(consulState)

	t.Run("ok", func(t *testing.T) {
		dc, ok := cfs.dc(agentconsul.Self{
			"Config": {"Datacenter": "dc1"},
		})
		must.True(t, ok)
		must.Eq(t, "dc1", dc)
	})

	t.Run("malformed", func(t *testing.T) {
		_, ok := cfs.dc(agentconsul.Self{
			"Config": {"Datacenter": 9000},
		})
		must.False(t, ok)
	})

	t.Run("missing", func(t *testing.T) {
		_, ok := cfs.dc(agentconsul.Self{
			"Config": {},
		})
		must.False(t, ok)
	})
}

func TestConsulFingerprint_segment(t *testing.T) {
	ci.Parallel(t)

	cfs := new(consulState)

	t.Run("ok", func(t *testing.T) {
		s, ok := cfs.segment(agentconsul.Self{
			"Member": {"Tags": map[string]interface{}{"segment": "seg1"}},
		})
		must.True(t, ok)
		must.Eq(t, "seg1", s)
	})

	t.Run("segment missing", func(t *testing.T) {
		_, ok := cfs.segment(agentconsul.Self{
			"Member": {"Tags": map[string]interface{}{}},
		})
		must.False(t, ok)
	})

	t.Run("tags missing", func(t *testing.T) {
		_, ok := cfs.segment(agentconsul.Self{
			"Member": {},
		})
		must.False(t, ok)
	})

	t.Run("malformed", func(t *testing.T) {
		_, ok := cfs.segment(agentconsul.Self{
			"Member": {"Tags": map[string]interface{}{"segment": 9000}},
		})
		must.False(t, ok)
	})
}

func TestConsulFingerprint_connect(t *testing.T) {
	ci.Parallel(t)

	cfs := new(consulState)

	t.Run("connect enabled", func(t *testing.T) {
		s, ok := cfs.connect(agentconsul.Self{
			"DebugConfig": {"ConnectEnabled": true},
		})
		must.True(t, ok)
		must.Eq(t, "true", s)
	})

	t.Run("connect not enabled", func(t *testing.T) {
		s, ok := cfs.connect(agentconsul.Self{
			"DebugConfig": {"ConnectEnabled": false},
		})
		must.True(t, ok)
		must.Eq(t, "false", s)
	})

	t.Run("connect missing", func(t *testing.T) {
		_, ok := cfs.connect(agentconsul.Self{
			"DebugConfig": {},
		})
		must.False(t, ok)
	})
}

func TestConsulFingerprint_grpc(t *testing.T) {
	ci.Parallel(t)

	cfs := new(consulState)

	t.Run("grpc set pre-1.14 http", func(t *testing.T) {
		s, ok := cfs.grpc("http", testlog.HCLogger(t))(agentconsul.Self{
			"Config":      {"Version": "1.13.3"},
			"DebugConfig": {"GRPCPort": 8502.0}, // JSON numbers are floats
		})
		must.True(t, ok)
		must.Eq(t, "8502", s)
	})

	t.Run("grpc disabled pre-1.14 http", func(t *testing.T) {
		s, ok := cfs.grpc("http", testlog.HCLogger(t))(agentconsul.Self{
			"Config":      {"Version": "1.13.3"},
			"DebugConfig": {"GRPCPort": -1.0}, // JSON numbers are floats
		})
		must.True(t, ok)
		must.Eq(t, "-1", s)
	})

	t.Run("grpc set pre-1.14 https", func(t *testing.T) {
		s, ok := cfs.grpc("https", testlog.HCLogger(t))(agentconsul.Self{
			"Config":      {"Version": "1.13.3"},
			"DebugConfig": {"GRPCPort": 8502.0}, // JSON numbers are floats
		})
		must.True(t, ok)
		must.Eq(t, "8502", s)
	})

	t.Run("grpc disabled pre-1.14 https", func(t *testing.T) {
		s, ok := cfs.grpc("https", testlog.HCLogger(t))(agentconsul.Self{
			"Config":      {"Version": "1.13.3"},
			"DebugConfig": {"GRPCPort": -1.0}, // JSON numbers are floats
		})
		must.True(t, ok)
		must.Eq(t, "-1", s)
	})

	t.Run("grpc set post-1.14 http", func(t *testing.T) {
		s, ok := cfs.grpc("http", testlog.HCLogger(t))(agentconsul.Self{
			"Config":      {"Version": "1.14.0"},
			"DebugConfig": {"GRPCPort": 8502.0}, // JSON numbers are floats
		})
		must.True(t, ok)
		must.Eq(t, "8502", s)
	})

	t.Run("grpc disabled post-1.14 http", func(t *testing.T) {
		s, ok := cfs.grpc("http", testlog.HCLogger(t))(agentconsul.Self{
			"Config":      {"Version": "1.14.0"},
			"DebugConfig": {"GRPCPort": -1.0}, // JSON numbers are floats
		})
		must.True(t, ok)
		must.Eq(t, "-1", s)
	})

	t.Run("grpc disabled post-1.14 https", func(t *testing.T) {
		s, ok := cfs.grpc("https", testlog.HCLogger(t))(agentconsul.Self{
			"Config":      {"Version": "1.14.0"},
			"DebugConfig": {"GRPCTLSPort": -1.0}, // JSON numbers are floats
		})
		must.True(t, ok)
		must.Eq(t, "-1", s)
	})

	t.Run("grpc set post-1.14 https", func(t *testing.T) {
		s, ok := cfs.grpc("https", testlog.HCLogger(t))(agentconsul.Self{
			"Config":      {"Version": "1.14.0"},
			"DebugConfig": {"GRPCTLSPort": 8503.0}, // JSON numbers are floats
		})
		must.True(t, ok)
		must.Eq(t, "8503", s)
	})

	t.Run("version with extra spaces", func(t *testing.T) {
		s, ok := cfs.grpc("https", testlog.HCLogger(t))(agentconsul.Self{
			"Config":      {"Version": "  1.14.0\n"},
			"DebugConfig": {"GRPCTLSPort": 8503.0}, // JSON numbers are floats
		})
		must.True(t, ok)
		must.Eq(t, "8503", s)
	})

	t.Run("grpc missing http", func(t *testing.T) {
		_, ok := cfs.grpc("http", testlog.HCLogger(t))(agentconsul.Self{
			"DebugConfig": {},
		})
		must.False(t, ok)
	})

	t.Run("grpc missing https", func(t *testing.T) {
		_, ok := cfs.grpc("https", testlog.HCLogger(t))(agentconsul.Self{
			"DebugConfig": {},
		})
		must.False(t, ok)
	})
}

func TestConsulFingerprint_namespaces(t *testing.T) {
	ci.Parallel(t)

	cfs := new(consulState)

	t.Run("supports namespaces", func(t *testing.T) {
		value, ok := cfs.namespaces(agentconsul.Self{
			"Stats": {"license": map[string]interface{}{"features": "Automated Backups, Automated Upgrades, Enhanced Read Scalability, Network Segments, Redundancy Zone, Advanced Network Federation, Namespaces, SSO, Audit Logging"}},
		})
		must.True(t, ok)
		must.Eq(t, "true", value)
	})

	t.Run("no namespaces", func(t *testing.T) {
		value, ok := cfs.namespaces(agentconsul.Self{
			"Stats": {"license": map[string]interface{}{"features": "Automated Backups, Automated Upgrades, Enhanced Read Scalability, Network Segments, Redundancy Zone, Advanced Network Federation, SSO, Audit Logging"}},
		})
		must.True(t, ok)
		must.Eq(t, "false", value)

	})

	t.Run("stats missing", func(t *testing.T) {
		value, ok := cfs.namespaces(agentconsul.Self{})
		must.True(t, ok)
		must.Eq(t, "false", value)
	})

	t.Run("license missing", func(t *testing.T) {
		value, ok := cfs.namespaces(agentconsul.Self{"Stats": {}})
		must.True(t, ok)
		must.Eq(t, "false", value)
	})

	t.Run("features missing", func(t *testing.T) {
		value, ok := cfs.namespaces(agentconsul.Self{"Stats": {"license": map[string]interface{}{}}})
		must.True(t, ok)
		must.Eq(t, "false", value)
	})
}

func TestConsulFingerprint_partition(t *testing.T) {
	ci.Parallel(t)

	cfs := new(consulState)

	t.Run("oss", func(t *testing.T) {
		p, ok := cfs.partition(agentconsul.Self{
			"Config": {"Version": "v1.9.5"},
		})
		must.True(t, ok)
		must.Eq(t, "", p)
	})

	t.Run("ent default partition", func(t *testing.T) {
		p, ok := cfs.partition(agentconsul.Self{
			"Config": {"Version": "v1.9.5+ent"},
		})
		must.True(t, ok)
		must.Eq(t, "default", p)
	})

	t.Run("ent nondefault partition", func(t *testing.T) {
		p, ok := cfs.partition(agentconsul.Self{
			"Config": {"Version": "v1.9.5+ent", "Partition": "test"},
		})
		must.True(t, ok)
		must.Eq(t, "test", p)
	})

	t.Run("missing", func(t *testing.T) {
		p, ok := cfs.partition(agentconsul.Self{
			"Config": {},
		})
		must.True(t, ok)
		must.Eq(t, "", p)
	})

	t.Run("malformed", func(t *testing.T) {
		p, ok := cfs.partition(agentconsul.Self{
			"Config": {"Version": "***"},
		})
		must.True(t, ok)
		must.Eq(t, "", p)
	})
}

func TestConsulFingerprint_dns(t *testing.T) {
	ci.Parallel(t)

	cfs := new(consulState)

	t.Run("dns port not enabled", func(t *testing.T) {
		port, ok := cfs.dnsPort(agentconsul.Self{
			"DebugConfig": {"DNSPort": -1.0}, // JSON numbers are floats
		})
		must.True(t, ok)
		must.Eq(t, "-1", port)
	})

	t.Run("non-default port value", func(t *testing.T) {
		port, ok := cfs.dnsPort(agentconsul.Self{
			"DebugConfig": {"DNSPort": 8601.0}, // JSON numbers are floats
		})
		must.True(t, ok)
		must.Eq(t, "8601", port)
	})

	t.Run("missing port", func(t *testing.T) {
		port, ok := cfs.dnsPort(agentconsul.Self{
			"DebugConfig": {},
		})
		must.False(t, ok)
		must.Eq(t, "0", port)
	})

	t.Run("malformed port", func(t *testing.T) {
		port, ok := cfs.dnsPort(agentconsul.Self{
			"DebugConfig": {"DNSPort": "A"},
		})
		must.False(t, ok)
		must.Eq(t, "0", port)
	})

	t.Run("get first IP", func(t *testing.T) {
		addr, ok := cfs.dnsAddr(testlog.HCLogger(t))(agentconsul.Self{
			"DebugConfig": {
				"DNSAddrs": []any{"tcp://192.168.1.170:8601", "udp://192.168.1.171:8601"},
			},
		})
		must.True(t, ok)
		must.Eq(t, "192.168.1.170", addr)

		addr, ok = cfs.dnsAddr(testlog.HCLogger(t))(agentconsul.Self{
			"DebugConfig": {"DNSAddrs": []any{"tcp://[2001:0db8:85a3::8a2e:0370:7334]:8601"}},
		})
		must.True(t, ok)
		must.Eq(t, "2001:db8:85a3::8a2e:370:7334", addr)
	})

	t.Run("loopback address", func(t *testing.T) {
		addr, ok := cfs.dnsAddr(testlog.HCLogger(t))(agentconsul.Self{
			"DebugConfig": {
				"DNSAddrs": []any{"tcp://127.0.0.1:8601", "udp://127.0.0.1:8601"},
			},
		})
		must.True(t, ok)
		must.Eq(t, "", addr)

		addr, ok = cfs.dnsAddr(testlog.HCLogger(t))(agentconsul.Self{
			"DebugConfig": {"DNSAddrs": []any{"tcp://[::1]:8601"}},
		})
		must.True(t, ok)
		must.Eq(t, "", addr)

	})

	t.Run("fallback to private or public IP", func(t *testing.T) {
		addr, ok := cfs.dnsAddr(testlog.HCLogger(t))(agentconsul.Self{
			"DebugConfig": {
				"DNSAddrs": []any{"tcp://0.0.0.0:8601", "udp://0.0.0.0:8601"},
			},
		})
		must.True(t, ok)
		must.NotEq(t, "", addr)
	})

	t.Run("malformed DNSAddrs", func(t *testing.T) {
		addr, ok := cfs.dnsAddr(testlog.HCLogger(t))(agentconsul.Self{
			"DebugConfig": {"DNSAddrs": []int{0}}})
		must.False(t, ok)
		must.Eq(t, "", addr)

		addr, ok = cfs.dnsAddr(testlog.HCLogger(t))(agentconsul.Self{
			"DebugConfig": {"DNSAddrs": []any{0}}})
		must.False(t, ok)
		must.Eq(t, "", addr)

		addr, ok = cfs.dnsAddr(testlog.HCLogger(t))(agentconsul.Self{
			"DebugConfig": {"DNSAddrs": []any{"tcp://XXXXX"}}})
		must.False(t, ok)
		must.Eq(t, "", addr)
	})

}

func TestConsulFingerprint_Fingerprint_oss(t *testing.T) {
	ci.Parallel(t)

	cf := newConsulFingerPrint(t)

	ts, cfg := fakeConsul(fakeConsulPayload(t, "test_fixtures/consul/agent_self_ce.json"))
	defer ts.Close()

	node := &structs.Node{Attributes: make(map[string]string)}

	// consul not available before first run
	must.Nil(t, cf.clusters[structs.ConsulDefaultCluster])

	// execute first query with good response
	var resp FingerprintResponse
	err := cf.Fingerprint(&FingerprintRequest{Config: cfg, Node: node}, &resp)
	must.NoError(t, err)
	must.Eq(t, map[string]string{
		"consul.datacenter":    "dc1",
		"consul.dns.port":      "8600",
		"consul.revision":      "3c1c22679",
		"consul.segment":       "seg1",
		"consul.server":        "true",
		"consul.sku":           "oss",
		"consul.version":       "1.9.5",
		"consul.connect":       "true",
		"consul.grpc":          "8502",
		"consul.ft.namespaces": "false",
		"unique.consul.name":   "HAL9000",
	}, resp.Attributes)
	must.True(t, resp.Detected)

	// consul now available
	must.NotNil(t, cf.clusters[structs.ConsulDefaultCluster])

	var resp2 FingerprintResponse

	// pretend attributes set for failing request
	node.Attributes["consul.datacenter"] = "foo"
	node.Attributes["consul.revision"] = "foo"
	node.Attributes["consul.segment"] = "foo"
	node.Attributes["consul.server"] = "foo"
	node.Attributes["consul.sku"] = "foo"
	node.Attributes["consul.version"] = "foo"
	node.Attributes["consul.connect"] = "foo"
	node.Attributes["connect.grpc"] = "foo"
	node.Attributes["unique.consul.name"] = "foo"

	// execute second query with error but without reload
	err2 := cf.Fingerprint(&FingerprintRequest{Config: cfg, Node: node}, &resp2)
	must.NoError(t, err2)                         // does not return error
	must.Eq(t, resp.Attributes, resp2.Attributes) // attributes unset don't change
	must.True(t, resp.Detected)                   // never downgrade

	// Consul no longer available but an agent reload is required to clear a
	// detected cluster
	must.NotNil(t, cf.clusters[structs.ConsulDefaultCluster])

	// Reload to reset
	cf.Reload()
	var resp3 FingerprintResponse
	err3 := cf.Fingerprint(&FingerprintRequest{Config: cfg, Node: node}, &resp3)
	must.NoError(t, err3)         // does not return error
	must.Nil(t, resp3.Attributes) // attributes reset
	must.True(t, resp3.Detected)  // detection is never reset

	// execute 4th query no error
	var resp4 FingerprintResponse
	err4 := cf.Fingerprint(&FingerprintRequest{Config: cfg, Node: node}, &resp4)
	must.NoError(t, err4)
	must.Eq(t, map[string]string{
		"consul.datacenter":    "dc1",
		"consul.revision":      "3c1c22679",
		"consul.segment":       "seg1",
		"consul.server":        "true",
		"consul.sku":           "oss",
		"consul.version":       "1.9.5",
		"consul.connect":       "true",
		"consul.grpc":          "8502",
		"consul.dns.port":      "8600",
		"consul.ft.namespaces": "false",
		"unique.consul.name":   "HAL9000",
	}, resp4.Attributes)

	// consul now available again
	must.NotNil(t, cf.clusters[structs.ConsulDefaultCluster])
	must.True(t, resp.Detected)
}

func TestConsulFingerprint_Fingerprint_ent(t *testing.T) {
	ci.Parallel(t)

	cf := newConsulFingerPrint(t)

	ts, cfg := fakeConsul(fakeConsulPayload(t, "test_fixtures/consul/agent_self_ent.json"))
	defer ts.Close()

	node := &structs.Node{Attributes: make(map[string]string)}

	// consul not available before first run
	must.Nil(t, cf.clusters[structs.ConsulDefaultCluster])

	// execute first query with good response
	var resp FingerprintResponse
	err := cf.Fingerprint(&FingerprintRequest{Config: cfg, Node: node}, &resp)
	must.NoError(t, err)
	must.Eq(t, map[string]string{
		"consul.datacenter":      "dc1",
		"consul.revision":        "22ce6c6ad",
		"consul.segment":         "seg1",
		"consul.server":          "true",
		"consul.sku":             "ent",
		"consul.version":         "1.9.5+ent",
		"consul.ft.namespaces":   "true",
		"consul.connect":         "true",
		"consul.grpc":            "8502",
		"unique.consul.dns.addr": "192.168.1.117",
		"consul.dns.port":        "8600",
		"consul.partition":       "default",
		"unique.consul.name":     "HAL9000",
	}, resp.Attributes)
	must.True(t, resp.Detected)

	// consul now available
	must.NotNil(t, cf.clusters[structs.ConsulDefaultCluster])

	var resp2 FingerprintResponse

	// pretend attributes set for failing request
	node.Attributes["consul.datacenter"] = "foo"
	node.Attributes["consul.revision"] = "foo"
	node.Attributes["consul.segment"] = "foo"
	node.Attributes["consul.server"] = "foo"
	node.Attributes["consul.sku"] = "foo"
	node.Attributes["consul.version"] = "foo"
	node.Attributes["consul.ft.namespaces"] = "foo"
	node.Attributes["consul.connect"] = "foo"
	node.Attributes["connect.grpc"] = "foo"
	node.Attributes["unique.consul.name"] = "foo"

	// execute second query with error but without reload
	err2 := cf.Fingerprint(&FingerprintRequest{Config: cfg, Node: node}, &resp2)
	must.NoError(t, err2)                         // does not return error
	must.Eq(t, resp.Attributes, resp2.Attributes) // attributes unset don't change
	must.True(t, resp.Detected)                   // never downgrade

	// Consul no longer available but an agent reload is required to clear a
	// detected cluster
	must.NotNil(t, cf.clusters[structs.ConsulDefaultCluster])

	// Reload to reset
	cf.Reload()
	var resp3 FingerprintResponse
	err3 := cf.Fingerprint(&FingerprintRequest{Config: cfg, Node: node}, &resp3)
	must.NoError(t, err3)         // does not return error
	must.Nil(t, resp3.Attributes) // attributes reset
	must.True(t, resp3.Detected)  // detection is never reset

	// execute 4th query no error
	var resp4 FingerprintResponse
	err4 := cf.Fingerprint(&FingerprintRequest{Config: cfg, Node: node}, &resp4)
	must.NoError(t, err4)
	must.Eq(t, map[string]string{
		"consul.datacenter":      "dc1",
		"consul.revision":        "22ce6c6ad",
		"consul.segment":         "seg1",
		"consul.server":          "true",
		"consul.sku":             "ent",
		"consul.version":         "1.9.5+ent",
		"consul.ft.namespaces":   "true",
		"consul.connect":         "true",
		"consul.grpc":            "8502",
		"unique.consul.dns.addr": "192.168.1.117",
		"consul.dns.port":        "8600",
		"consul.partition":       "default",
		"unique.consul.name":     "HAL9000",
	}, resp4.Attributes)

	// consul now available again
	must.NotNil(t, cf.clusters[structs.ConsulDefaultCluster])
	must.True(t, resp4.Detected)
}
