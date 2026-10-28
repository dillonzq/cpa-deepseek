package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	"github.com/dillonzq/apix/reasoning"
	"github.com/dillonzq/cpa-deepseek/internal/catalog"
	"github.com/dillonzq/cpa-deepseek/internal/errclass"
	"github.com/dillonzq/cpa-deepseek/internal/protocol"
)

func (r executorRequest) effectivePayload() []byte {
	if r.Payload != nil {
		return r.Payload
	}
	return r.OriginalRequest
}
func (r executorRequest) inputFormat() string {
	if r.SourceFormat != "" {
		return r.SourceFormat
	}
	if r.Format != "" {
		return r.Format
	}
	return "openai"
}
func (r executorRequest) outputFormat() string {
	if r.Format != "" {
		return r.Format
	}
	return r.inputFormat()
}

func selectRoute(_ catalog.ModelRecord, format string) catalog.Target {
	switch format {
	case "claude":
		return catalog.Target{Protocol: catalog.RouteMessages}
	case "openai-response":
		return catalog.Target{Protocol: catalog.RouteResponses}
	default:
		return catalog.Target{Protocol: catalog.RouteChatCompletions}
	}
}

func buildUpstreamBody(res *resolvedExecution, req executorRequest) ([]byte, *errclass.Error) {
	if _, e := protocol.Format(req.inputFormat()); e != nil {
		return nil, e
	}
	source := req.effectivePayload()
	if res.suffix.Effort != "" {
		target, _ := protocol.Route(res.route.Protocol)
		from, _ := protocol.Format(req.inputFormat())
		source = reasoning.StripSupersededReasoning(target, from, source)
	}
	// Native requests retain every documented field, including server tools,
	// reasoning history and future extensions. Only Gemini needs translation.
	body := source
	if req.inputFormat() == "gemini" {
		var e *errclass.Error
		body, e = protocol.ConvertRequest(res.route.Protocol, res.rec.UpstreamID, req.inputFormat(), source, res.state)
		if e != nil {
			return nil, e
		}
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(body, &doc) != nil || doc == nil {
		return nil, errclass.Translation("request must be a JSON object")
	}
	if res.route.Protocol == catalog.RouteResponses {
		for _, field := range []string{"previous_response_id", "conversation"} {
			raw := bytes.TrimSpace(doc[field])
			if len(raw) > 0 && string(raw) != "null" && string(raw) != `""` {
				return nil, errclass.Translation("DeepSeek Responses is stateless; send the complete input history instead of " + field)
			}
		}
	}
	doc["model"], _ = json.Marshal(res.rec.UpstreamID)
	doc["stream"], _ = json.Marshal(res.state.Stream)
	if e := normalizeThinking(res.route.Protocol, doc, res.suffix.Effort); e != nil {
		return nil, e
	}
	out, _ := json.Marshal(doc)
	return out, nil
}

func normalizeThinking(route catalog.Route, doc map[string]json.RawMessage, suffix string) *errclass.Error {
	field, container := "reasoning_effort", ""
	switch route {
	case catalog.RouteResponses:
		field, container = "effort", "reasoning"
	case catalog.RouteMessages:
		field, container = "effort", "output_config"
	}
	parent := doc
	if container != "" {
		parent = make(map[string]json.RawMessage)
		if raw := doc[container]; len(raw) > 0 && string(raw) != "null" {
			if json.Unmarshal(raw, &parent) != nil || parent == nil {
				return errclass.Translation("invalid reasoning controls")
			}
		}
	}
	effort := suffix
	if effort == "" {
		if raw := parent[field]; len(raw) > 0 && string(raw) != "null" {
			if json.Unmarshal(raw, &effort) != nil {
				return errclass.Translation("reasoning effort must be a string")
			}
		}
	}
	if effort == "" {
		return nil
	}
	level, ok := effortLevel(effort)
	if !ok {
		return errclass.Translation("unsupported DeepSeek reasoning effort")
	}
	parent[field], _ = json.Marshal(level)
	// An effort level must not overwrite a native thinking toggle. Only an
	// explicit none control or a model suffix needs to change that toggle.
	if route != catalog.RouteResponses && (level == "none" || suffix != "") {
		typ := "enabled"
		if level == "none" {
			typ = "disabled"
		}
		thinking := make(map[string]json.RawMessage)
		if raw := doc["thinking"]; len(raw) > 0 && string(raw) != "null" {
			if json.Unmarshal(raw, &thinking) != nil || thinking == nil {
				return errclass.Translation("invalid thinking control")
			}
		}
		thinking["type"], _ = json.Marshal(typ)
		doc["thinking"], _ = json.Marshal(thinking)
	}
	// Anthropic output_config.effort accepts enabled effort levels only.
	if route == catalog.RouteMessages && level == "none" {
		delete(parent, field)
	}
	if container != "" {
		if len(parent) == 0 {
			delete(doc, container)
		} else {
			doc[container], _ = json.Marshal(parent)
		}
	}
	return nil
}

func effortLevel(effort string) (string, bool) {
	switch strings.ToLower(effort) {
	case "none":
		return "none", true
	case "minimal", "low":
		return "low", true
	case "auto", "medium", "high", "xhigh":
		return "high", true
	case "max", "ultra":
		return "max", true
	default:
		return "", false
	}
}

func selectedStatusError(status int, body []byte, key string) *errclass.Error {
	if key != "" {
		body = bytes.ReplaceAll(body, []byte(key), []byte("[redacted]"))
	}
	return errclass.UpstreamStatusError(status, body)
}
func classified(err error) *errclass.Error {
	var result *errclass.Error
	if errors.As(err, &result) {
		return result
	}
	return &errclass.Error{Class: errclass.ClassUpstream, StatusCode: 502, Message: "DeepSeek operation failed"}
}
