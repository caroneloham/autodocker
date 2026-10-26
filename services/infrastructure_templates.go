package services

import "strings"

// The model chooses services; proven built-in templates own their default
// credentials, ports and volumes. Only requested customization overrides them.
func normalizeInfrastructureTemplate(prompt string, service *ServiceEntry, setting infrastructureSetting, catalogue map[string]string, mentioned map[string]bool) (bool, bool) {
	if _, builtin := ImagesBase[service.Key]; !builtin {
		return true, false
	}
	words := infrastructureWords(prompt)
	hasExplicit := false
	for _, wanted := range mentioned {
		if wanted {
			hasExplicit = true
		}
	}
	if !mentioned[service.Key] && hasExplicit {
		switch service.Key {
		case "portainer":
			if !strings.Contains(words, "gestion des conteneurs") && !strings.Contains(words, "gestion docker") {
				return false, true
			}
		case "traefik":
			if !strings.Contains(words, "proxy") && !strings.Contains(words, "routage") {
				return false, true
			}
		case "nginx":
			if (mentioned["wordpress"] || mentioned["nextcloud"] || mentioned["gitea"] || mentioned["moodle"]) && !strings.Contains(words, "proxy") {
				return false, true
			}
		}
	}
	changed := false
	if setting.Version == "" && service.Image != "" && !strings.Contains(strings.ToLower(prompt), strings.ToLower(service.Image)) {
		changed = service.Image != catalogue[service.Key]
		service.Image = ""
	}
	wantsEnv := false
	for _, term := range []string{"configur", "variable", "debug", "mot de passe", "identifiant", "utilisateur"} {
		if strings.Contains(words, term) {
			wantsEnv = true
		}
	}
	if !wantsEnv {
		for key := range service.Env {
			if !strings.Contains(strings.ToLower(prompt), strings.ToLower(key)) {
				delete(service.Env, key)
				changed = true
			}
		}
	}
	if setting.HostPort == 0 && !strings.Contains(words, "port") && len(service.Ports) > 0 {
		service.Ports = nil
		changed = true
	}
	if !strings.Contains(words, "volume") && !strings.Contains(words, "sauvegard") && !strings.Contains(words, "persistan") && len(service.Volumes) > 0 {
		service.Volumes = nil
		changed = true
	}
	if !strings.Contains(words, "command") && !strings.Contains(words, "argument") && len(service.Command) > 0 {
		service.Command = nil
		changed = true
	}
	return true, changed
}
