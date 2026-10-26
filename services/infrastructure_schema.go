package services

// LM Studio supports grammar-constrained JSON. Other providers still share
// the exact same contract and validator without requiring this extension.
func infrastructureResponseSchema() map[string]interface{} {
	text := map[string]interface{}{"type": "string"}
	texts := map[string]interface{}{"type": "array", "items": text}
	dict := map[string]interface{}{"type": "object", "additionalProperties": text}
	service := map[string]interface{}{"type": "object", "additionalProperties": false, "required": []string{"key", "image"}, "properties": map[string]interface{}{
		"key": text, "network": text, "ip": text, "image": text, "env": dict, "extra": dict,
		"ports": texts, "volumes": texts, "command": texts, "depends_on": texts,
	}}
	network := map[string]interface{}{"type": "object", "additionalProperties": false, "required": []string{"name", "cidr", "gateway"}, "properties": map[string]interface{}{"name": text, "cidr": text, "gateway": text}}
	flags := map[string]interface{}{}
	for _, key := range []string{"socle", "web", "db", "collab", "observ", "ai"} {
		flags[key] = map[string]interface{}{"type": "boolean"}
	}
	return map[string]interface{}{"type": "object", "additionalProperties": false, "required": []string{"services"}, "properties": map[string]interface{}{
		"ai_stack": map[string]interface{}{"type": "object", "additionalProperties": false, "required": []string{"engine", "model"}, "properties": map[string]interface{}{
			"engine": map[string]interface{}{"type": "string", "enum": []string{"vllm", "ollama"}}, "model": text, "embedding_model": text, "tool_parser": text, "reasoning_parser": text, "vision": map[string]interface{}{"type": "boolean"}, "private_platform": map[string]interface{}{"type": "boolean"}, "admin_email": text, "max_context": map[string]interface{}{"type": "integer", "minimum": 512, "maximum": 131072}, "concurrency": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": 16}, "gpu_memory": map[string]interface{}{"type": "number", "minimum": 0.2, "maximum": 0.95},
		}},
		"ingress":  map[string]interface{}{"type": "object", "additionalProperties": false, "required": []string{"proxy", "domain", "port"}, "properties": map[string]interface{}{"proxy": map[string]interface{}{"type": "string", "enum": []string{"caddy", "traefik", "haproxy"}}, "domain": text, "port": map[string]interface{}{"type": "integer", "minimum": 1024, "maximum": 65535}, "tls": map[string]interface{}{"type": "string", "enum": []string{"", "internal"}}}},
		"services": map[string]interface{}{"type": "array", "maxItems": 64, "items": service},
		"summary":  text, "reasons": dict, "questions": map[string]interface{}{"type": "array", "maxItems": 3, "items": text}, "warnings": texts,
		"addressing": map[string]interface{}{"type": "object", "additionalProperties": false, "required": []string{"networks"}, "properties": map[string]interface{}{"networks": map[string]interface{}{"type": "array", "maxItems": 16, "items": network}}},
		"ui":         map[string]interface{}{"type": "object", "additionalProperties": false, "properties": flags},
	}}
}
