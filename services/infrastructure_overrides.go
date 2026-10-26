package services

import (
	"fmt"
	"sort"
	"strings"
)

// Apply the same supported settings to built-in services as to freeform ones.
// Previously the JSON retained those settings but the Compose builder ignored
// them for built-ins. Existing connections and data volumes remain defaults.
func applyBuiltinInfrastructureOverrides(services M, entries map[string]*ServiceEntry, builtins map[string]bool, volumes M, ports *buildPortTracker, autoPassword string) {
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		entry := entries[key]
		if !builtins[key] {
			continue
		}
		svc, ok := services[key].(M)
		if !ok {
			continue
		}
		if entry.Image != "" && ValidFreeformImage(entry.Image) {
			svc["image"] = entry.Image
		}
		if len(entry.Env) > 0 {
			env, _ := svc["environment"].(M)
			if env == nil {
				env = M{}
			}
			for k, value := range entry.Env {
				if !validEnvKeyRe.MatchString(k) {
					continue
				}
				if value == AutoPasswordPlaceholder {
					// Preserve the builder's shared application/database credential.
					if current, ok := env[k].(string); ok && current != "" {
						value = current
					} else {
						value = autoPassword
					}
				} else {
					value = strings.ReplaceAll(value, AutoPasswordPlaceholder, autoPassword)
				}
				env[k] = value
			}
			svc["environment"] = env
		}
		if len(entry.Ports) > 0 {
			valid := []string{}
			for _, p := range entry.Ports {
				if _, _, ok := sanitizePortSpec(p); ok {
					valid = append(valid, p)
				}
			}
			if len(valid) > 0 {
				for _, old := range toStringList(svc["ports"]) {
					if host, _, ok := sanitizePortSpec(old); ok {
						delete(ports.used, host)
					}
				}
				updated := []string{}
				for _, p := range valid {
					host, target, _ := sanitizePortSpec(p)
					updated = append(updated, fmt.Sprintf("%d:%d", ports.getFreePort(host), target))
				}
				svc["ports"] = updated
			}
		}
		if len(entry.Volumes) > 0 {
			updated := toStringList(svc["volumes"])
			for _, v := range entry.Volumes {
				name, target, ok := sanitizeNamedVolume(v)
				if !ok || strings.Contains(strings.ToLower(target), "docker.sock") {
					continue
				}
				matched := false
				for i, old := range updated {
					_, oldTarget, ok := sanitizeNamedVolume(old)
					if ok && oldTarget == target {
						updated[i] = name + ":" + target
						matched = true
						break
					}
				}
				if !matched {
					updated = append(updated, name+":"+target)
				}
				volumes[name] = M{}
			}
			svc["volumes"] = updated
		}
		if len(entry.Command) > 0 {
			command := make([]string, len(entry.Command))
			for i, arg := range entry.Command {
				command[i] = strings.ReplaceAll(arg, AutoPasswordPlaceholder, autoPassword)
			}
			svc["command"] = command
		}
		deps := toStringList(svc["depends_on"])
		for _, dep := range entry.DependsOn {
			if dep != key && services[dep] != nil && !containsInfrastructureString(deps, dep) {
				deps = append(deps, dep)
			}
		}
		if len(deps) > 0 {
			svc["depends_on"] = deps
		}
	}
}
