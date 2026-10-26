package services

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Freeform services — services proposed by the AI outside the built-in
// catalogue, so that any Docker infrastructure can be generated from a single
// prompt. All input is untrusted: every field is validated before being
// turned into compose configuration. Dangerous compose options (privileged,
// cap_add, host network mode, bind mounts, devices, pid/ipc) are never
// honoured — only image, environment, ports, named volumes, command and
// depends_on pass through.
// ---------------------------------------------------------------------------

// AutoPasswordPlaceholder is replaced by a generated password shared by all
// freeform services of the same deployment (so an app and its DB stay in sync).
const AutoPasswordPlaceholder = "__AUTO_PASSWORD__"

// minFreeformHostPort avoids exposing privileged host ports for AI-proposed services.
const minFreeformHostPort = 1024

// defaultFreeformPort is the fallback host port target for custom catalogue services.
const defaultFreeformPort = 8090

var (
	validImageRe   = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*(?::[A-Za-z0-9][A-Za-z0-9._-]{0,127})?(?:@sha256:[a-f0-9]{64})?$`)
	validEnvKeyRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)
	validVolNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	validVolPathRe = regexp.MustCompile(`^/[^:]+$`)
)

// ValidFreeformImage reports whether an image reference is acceptable
// (lowercase repo, optional tag or digest, no shell metacharacters).
func ValidFreeformImage(image string) bool {
	if image == "" || len(image) > 255 {
		return false
	}
	return validImageRe.MatchString(image)
}

// sanitizePortSpec validates "host:container" or "container" specs and
// returns (hostTarget, containerPort, ok).
func sanitizePortSpec(spec string) (int, int, bool) {
	parsePort := func(s string) (int, bool) {
		p, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || p < 1 || p > 65535 {
			return 0, false
		}
		return p, true
	}
	parts := strings.Split(strings.TrimSpace(spec), ":")
	switch len(parts) {
	case 1:
		cp, ok := parsePort(parts[0])
		return cp, cp, ok
	case 2:
		hp, ok1 := parsePort(parts[0])
		cp, ok2 := parsePort(parts[1])
		if !ok1 || !ok2 {
			return 0, 0, false
		}
		return hp, cp, true
	default:
		return 0, 0, false
	}
}

// sanitizeNamedVolume validates a "volume_name:/container/path" spec.
// Bind mounts (host paths, relative paths, docker.sock) are rejected because
// the source must be a plain volume name.
func sanitizeNamedVolume(spec string) (name, target string, ok bool) {
	name, target, found := strings.Cut(strings.TrimSpace(spec), ":")
	if !found || !validVolNameRe.MatchString(name) || !validVolPathRe.MatchString(target) || strings.Contains(target, "..") {
		return "", "", false
	}
	return name, target, true
}

// buildCustomService converts a non-catalogue ServiceEntry into a compose
// service. It accepts either a custom catalogue entry (image resolved by key)
// or a freeform entry (explicit image from the AI).
func buildCustomService(project string, s *ServiceEntry, pt *buildPortTracker, known map[string]bool, autoPw string) (M, []string, bool) {
	if s.Key == "" || cleanStr(s.Key) != s.Key {
		return nil, nil, false
	}
	fromCatalogue := GetImage(s.Key) != ""
	img := GetImage(s.Key)
	if img == "" {
		img = strings.TrimSpace(s.Image)
	}
	if !ValidFreeformImage(img) {
		return nil, nil, false
	}

	svc := M{
		"image":          img,
		"container_name": project + "-" + s.Key,
		"restart":        "unless-stopped",
	}

	if len(s.Env) > 0 {
		env := M{}
		for k, v := range s.Env {
			if !validEnvKeyRe.MatchString(k) {
				continue
			}
			env[k] = strings.ReplaceAll(v, AutoPasswordPlaceholder, autoPw)
		}
		if len(env) > 0 {
			svc["environment"] = env
		}
	}

	// Ports: the host side is always (re)allocated from the requested target
	// to avoid conflicts with running containers and other compose services.
	var ports []string
	for _, p := range s.Ports {
		target, containerPort, ok := sanitizePortSpec(p)
		if !ok {
			continue
		}
		if target < minFreeformHostPort {
			target = defaultFreeformPort
		}
		hp := pt.getFreePort(target)
		ports = append(ports, fmt.Sprintf("%d:%d", hp, containerPort))
	}
	// Custom catalogue services keep their historical default exposure.
	if len(ports) == 0 && fromCatalogue {
		hp := pt.getFreePort(defaultFreeformPort)
		ports = append(ports, fmt.Sprintf("%d:80", hp))
	}
	if len(ports) > 0 {
		svc["ports"] = ports
	}

	var volumes []string
	var volNames []string
	for _, v := range s.Volumes {
		name, target, ok := sanitizeNamedVolume(v)
		if !ok {
			continue
		}
		volumes = append(volumes, name+":"+target)
		volNames = append(volNames, name)
	}
	// Custom catalogue services keep their historical default data volume.
	if len(volumes) == 0 && fromCatalogue {
		volName := s.Key + "_data"
		volumes = append(volumes, volName+":/data")
		volNames = append(volNames, volName)
	}
	if len(volumes) > 0 {
		svc["volumes"] = volumes
	}

	if len(s.Command) > 0 {
		cmd := make([]string, 0, len(s.Command))
		for _, c := range s.Command {
			cmd = append(cmd, strings.ReplaceAll(c, AutoPasswordPlaceholder, autoPw))
		}
		svc["command"] = cmd
	}

	var deps []string
	for _, d := range s.DependsOn {
		d = cleanStr(d)
		if d != "" && d != s.Key && known[d] {
			deps = append(deps, d)
		}
	}
	if len(deps) > 0 {
		svc["depends_on"] = deps
	}

	return svc, volNames, true
}

// SanitizeFreeformSpec validates a freeform service map coming from the AI
// and normalizes its fields to the shapes BuildComposePayload expects.
func SanitizeFreeformSpec(key string, svc map[string]interface{}) (map[string]interface{}, bool) {
	if key == "" || cleanStr(key) != key {
		return nil, false
	}
	image, _ := svc["image"].(string)
	image = strings.TrimSpace(image)
	if !ValidFreeformImage(image) {
		return nil, false
	}

	out := map[string]interface{}{"key": key, "image": image}

	network := ""
	if raw, ok := svc["network"]; ok && raw != nil {
		network = strings.TrimSpace(fmt.Sprint(raw))
	}
	if network == "" {
		network = "back"
	}
	out["network"] = network

	if env, ok := svc["env"].(map[string]interface{}); ok {
		cleanEnv := map[string]string{}
		for k, v := range env {
			if validEnvKeyRe.MatchString(k) {
				cleanEnv[k] = fmt.Sprint(v)
			}
		}
		if len(cleanEnv) > 0 {
			out["env"] = cleanEnv
		}
	}

	if ports := toStringList(svc["ports"]); len(ports) > 0 {
		valid := make([]string, 0, len(ports))
		for _, p := range ports {
			if _, _, ok := sanitizePortSpec(p); ok {
				valid = append(valid, p)
			}
		}
		if len(valid) > 0 {
			out["ports"] = valid
		}
	}

	if vols := toStringList(svc["volumes"]); len(vols) > 0 {
		valid := make([]string, 0, len(vols))
		for _, v := range vols {
			if _, _, ok := sanitizeNamedVolume(v); ok {
				valid = append(valid, v)
			}
		}
		if len(valid) > 0 {
			out["volumes"] = valid
		}
	}

	if cmd := toStringList(svc["command"]); len(cmd) > 0 {
		out["command"] = cmd
	}

	if deps := toStringList(svc["depends_on"]); len(deps) > 0 {
		valid := make([]string, 0, len(deps))
		for _, d := range deps {
			if d != "" && cleanStr(d) == d {
				valid = append(valid, d)
			}
		}
		if len(valid) > 0 {
			out["depends_on"] = valid
		}
	}

	return out, true
}

// toStringList converts a JSON array (or string slice) into []string.
func toStringList(raw interface{}) []string {
	switch list := raw.(type) {
	case []string:
		return list
	case []interface{}:
		out := make([]string, 0, len(list))
		for _, item := range list {
			s := strings.TrimSpace(fmt.Sprint(item))
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}
