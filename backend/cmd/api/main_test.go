package main

import (
	"strings"
	"testing"

	"nyx/internal/platform/config"
)

func TestAgentMisconfiguration(t *testing.T) {
	validToken := strings.Repeat("a", 32)

	base := func() *config.Config {
		return &config.Config{
			PythonAgentURL:    "http://agent:9000",
			AgentServiceToken: validToken,
		}
	}

	cases := []struct {
		name     string
		mutate   func(*config.Config)
		wantErr  string
		wantNone bool
	}{
		{
			name:    "empty url wins over a short token",
			mutate:  func(c *config.Config) { c.PythonAgentURL = ""; c.AgentServiceToken = "short" },
			wantErr: "PYTHON_AGENT_URL is empty",
		},
		{
			name:    "short token",
			mutate:  func(c *config.Config) { c.AgentServiceToken = "short" },
			wantErr: "AGENT_SERVICE_TOKEN",
		},
		{
			name:    "plaintext url to a public host",
			mutate:  func(c *config.Config) { c.PythonAgentURL = "http://nyx-agent.onrender.com" },
			wantErr: "must use https",
		},
		{
			name:    "plaintext url to a public ipv4-mapped ipv6 literal",
			mutate:  func(c *config.Config) { c.PythonAgentURL = "http://[::ffff:808:808]:9000" },
			wantErr: "must use https",
		},
		{
			name:     "loopback url wires the proxy",
			mutate:   func(c *config.Config) { c.PythonAgentURL = "http://127.0.0.1:9100" },
			wantNone: true,
		},
		{
			name:     "https url wires the proxy",
			mutate:   func(c *config.Config) { c.PythonAgentURL = "https://nyx-agent.onrender.com" },
			wantNone: true,
		},
		{
			name: "allow-private opts in for a dotted host",
			mutate: func(c *config.Config) {
				c.PythonAgentURL = "http://agent.internal.example.com"
				c.AgentAllowPrivateURL = true
			},
			wantNone: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.mutate(cfg)
			err := agentMisconfiguration(cfg)
			if tc.wantNone {
				if err != nil {
					t.Fatalf("agentMisconfiguration() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("agentMisconfiguration() = nil, want error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("agentMisconfiguration() = %q, want it to mention %q", err.Error(), tc.wantErr)
			}
		})
	}
}
