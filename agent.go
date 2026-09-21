package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AgentClient talks to the telnetAgent running on each host:
//
//	POST http://<agent-ip>:29900/   {"address": "...", "port": "..."}
//	-> {"dst": "...", "port": ..., "src": "<agent ip>", "result": "Connected" | "Failed: ..."}
type AgentClient struct {
	http *http.Client
	port int
}

func NewAgentClient(port int, timeout time.Duration) *AgentClient {
	return &AgentClient{
		http: &http.Client{Timeout: timeout},
		port: port,
	}
}

type agentRequest struct {
	Address string `json:"address"`
	Port    string `json:"port"`
}

type agentResponse struct {
	Src    string `json:"src"`
	Result string `json:"result"`
}

// TelnetResult is one agent testing one dest:port.
type TelnetResult struct {
	Agent      string `json:"agent"`                 // IP of the agent that ran the test
	Dest       string `json:"dest"`                  // target that was tested
	Port       int    `json:"port"`                  // target port that was tested
	Reachable  bool   `json:"reachable"`             // true only if the agent reported "Connected"
	Result     string `json:"result,omitempty"`      // raw result string from the agent
	AgentSrc   string `json:"src,omitempty"`         // source IP the agent reported for itself
	AgentError string `json:"agent_error,omitempty"` // set if the agent itself couldn't be reached / misbehaved
	DurationMS int64  `json:"duration_ms"`
}

func (c *AgentClient) Check(ctx context.Context, agentIP, dest string, port int) TelnetResult {
	start := time.Now()
	res := c.check(ctx, agentIP, dest, port)
	res.DurationMS = time.Since(start).Milliseconds()
	return res
}

func (c *AgentClient) check(ctx context.Context, agentIP, dest string, port int) TelnetResult {
	res := TelnetResult{Agent: agentIP, Dest: dest, Port: port}

	payload, err := json.Marshal(agentRequest{Address: dest, Port: strconv.Itoa(port)})
	if err != nil {
		res.AgentError = err.Error()
		return res
	}

	endpoint := "http://" + net.JoinHostPort(agentIP, strconv.Itoa(c.port)) + "/"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		res.AgentError = err.Error()
		return res
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		res.AgentError = "agent unreachable: " + err.Error()
		return res
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		res.AgentError = "reading agent response: " + err.Error()
		return res
	}
	if resp.StatusCode != http.StatusOK {
		res.AgentError = fmt.Sprintf("agent returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		return res
	}

	var ar agentResponse
	if err := json.Unmarshal(body, &ar); err != nil {
		res.AgentError = "invalid agent response: " + err.Error()
		return res
	}
	res.Result = ar.Result
	res.AgentSrc = ar.Src
	res.Reachable = ar.Result == "Connected"
	return res
}

// CheckAll runs dest:port for every agent x port with bounded concurrency.
// Results are returned in a stable order: agent by agent, port by port.
func (c *AgentClient) CheckAll(ctx context.Context, agents []string, dest string, ports []int, maxConcurrency int) []TelnetResult {
	results := make([]TelnetResult, len(agents)*len(ports))
	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup

	for i, agentIP := range agents {
		for j, port := range ports {
			idx := i*len(ports) + j
			wg.Add(1)
			sem <- struct{}{}
			go func(idx int, agentIP string, port int) {
				defer wg.Done()
				defer func() { <-sem }()
				results[idx] = c.Check(ctx, agentIP, dest, port)
			}(idx, agentIP, port)
		}
	}
	wg.Wait()
	return results
}
