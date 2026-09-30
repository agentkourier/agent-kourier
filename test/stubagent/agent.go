package stubagent

import (
	"encoding/json"
	"net/http"
)

// Agent is one scripted agent. Register it with Server.Agent.
type Agent struct {
	s    *Server
	ns   string
	name string
}

// serveCard answers with the agent card, which lists the HITL extension as kagent's does.
func (a *Agent) serveCard(w http.ResponseWriter, r *http.Request) {
	card := obj{
		"name":        a.name,
		"description": "stubagent: a scripted stand-in for a kagent agent",
		"version":     "1.0.0",
		"supportedInterfaces": []obj{{
			"url": "http://" + r.Host + agentPath(a.ns, a.name), "protocolBinding": "JSONRPC", "protocolVersion": "1.0",
		}},
		"capabilities": obj{"streaming": true, "extensions": []obj{
			{"uri": HITLExtension, "description": "Human approval and user input"},
		}},
		"defaultInputModes":  []string{"text"},
		"defaultOutputModes": []string{"text"},
		"skills":             []obj{},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(card)
}

// serveRPC decodes one JSON-RPC call and dispatches it.
func (a *Agent) serveRPC(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	o := &rw{s: a.s, w: w}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		o.fail(&rpcError{codeParse, "parse error", "PARSE_ERROR"})
		return
	}
	o.id = req.ID
	o.fail(&rpcError{codeMethod, "method not found: " + req.Method, "METHOD_NOT_FOUND"})
}
