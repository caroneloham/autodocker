package services

import (
	_ "embed"
	"regexp"
	"strings"
)

//go:embed minecraft_assets/bridge.py
var minecraftChatBridge string

func managedMinecraftChatProposal(payload map[string]interface{}) bool {
	bridge := false
	for _, service := range normalizeServiceList(payload["services"]) {
		key, _ := service["key"].(string)
		switch key {
		case "minecraft-ai":
			bridge = true
		case "minecraft", "vllm", "ollama", "open-webui", "caddy":
		default:
			return false
		}
	}
	return bridge
}

func minecraftAIConnectionRequested(prompt string) bool {
	w := infrastructureWords(prompt)
	return strings.Contains(w, "minecraft") && regexp.MustCompile(`\b(?:ia|ai|assistant)\b`).MatchString(w) && regexp.MustCompile(`connect|interag|chat du jeu|dans le chat`).MatchString(w)
}

func minecraftAIChatRequested(prompt string) bool {
	if regexp.MustCompile(`\b(?:sans|pas de|without) (?:chat|ia|assistant)\b`).MatchString(infrastructureWords(prompt)) {
		return false
	}
	return minecraftAIConnectionRequested(prompt) && regexp.MustCompile(`\bchat\b|conversation|discussion`).MatchString(infrastructureWords(prompt))
}

func addMinecraftAIRequirement(prompt string, entries []map[string]interface{}) []map[string]interface{} {
	if !minecraftAIChatRequested(prompt) || len(entries) == 0 {
		return entries
	}
	seen := map[string]bool{}
	for _, s := range entries {
		key, _ := s["key"].(string)
		seen[canonicalInfrastructureKey(key)] = true
	}
	engine := "ollama"
	network := "app"
	if n, ok := entries[0]["network"].(string); ok && n != "" {
		network = n
	}
	requested := mentionedInfrastructureServices(prompt, professionalAIImages)
	if seen["vllm"] || requested["vllm"] {
		engine = "vllm"
	}
	for _, key := range []string{"minecraft", "minecraft-ai", engine} {
		if !seen[key] {
			entries = append(entries, map[string]interface{}{"key": key, "network": network})
		}
	}
	return entries
}

func applyMinecraftAIStack(compose M, c AIStackConfig, model, base, key string) []M {
	svcs := compose["services"].(M)
	mc, ok := svcs["minecraft"].(M)
	if !ok || svcs[c.Engine] == nil {
		return nil
	}
	password := generatePassword(40)
	env, _ := mc["environment"].(M)
	if env == nil {
		env = M{}
		mc["environment"] = env
	}
	env["ENABLE_RCON"], env["RCON_PASSWORD"], env["RCON_PORT"] = "TRUE", password, "25575"
	env["OVERRIDE_SERVER_PROPERTIES"], env["SKIP_SERVER_PROPERTIES"] = "TRUE", "FALSE"
	delete(env, "RCON_PASSWORD_FILE")
	env["BROADCAST_RCON_TO_OPS"] = "FALSE"
	// Keep Minecraft's EULA choice intact. RCON is never published to the host.
	ports := []string{}
	for _, p := range toStringList(mc["ports"]) {
		_, inner, valid := infrastructurePublishedPort(p)
		if valid && inner != 25575 {
			ports = append(ports, p)
		}
	}
	mc["ports"] = ports
	networks, _ := mc["networks"].(M)
	if networks == nil {
		networks = M{}
		mc["networks"] = networks
	}
	networks["autodocker-ai"] = M{}
	volume := "minecraft_data"
	mounts := toStringList(mc["volumes"])
	found := false
	for _, mount := range mounts {
		parts := strings.Split(mount, ":")
		if len(parts) >= 2 && parts[1] == "/data" {
			volume = parts[0]
			found = true
			break
		}
	}
	if !found {
		compose["volumes"].(M)[volume] = M{}
		mc["volumes"] = append(mounts, volume+":/data")
	}
	deps := M{"minecraft": M{"condition": "service_started"}, c.Engine: M{"condition": "service_healthy"}}
	if c.Engine == "ollama" && svcs["ollama-model-init"] != nil {
		deps["ollama-model-init"] = M{"condition": "service_completed_successfully"}
	}
	svcs["minecraft-ai"] = M{
		"image": "python:3.13-alpine", "restart": "unless-stopped", "init": true,
		"command":     []string{"python", "-u", "-c", minecraftChatBridge},
		"environment": M{"AI_BASE_URL": base, "AI_MODEL": model, "AI_API_KEY": key, "RCON_PASSWORD": password, "PYTHONDONTWRITEBYTECODE": "1"},
		"volumes":     []string{volume + ":/minecraft:ro"}, "networks": M{"autodocker-ai": M{}}, "depends_on": deps,
		"read_only": true, "user": "65534:65534", "cap_drop": []string{"ALL"}, "security_opt": []string{"no-new-privileges:true"},
		"mem_limit": "128m", "cpus": 0.5, "pids_limit": 32,
	}
	return []M{{"from": "minecraft-ai", "to": c.Engine, "kind": "inference", "endpoint": base}, {"from": "minecraft-ai", "to": "minecraft", "kind": "minecraft-chat", "endpoint": "rcon://minecraft:25575 · @ia → réponse dans le chat"}}
}

func MinecraftAIProjectFiles(compose M) map[string]string {
	svcs, _ := compose["services"].(M)
	if svcs["minecraft-ai"] == nil {
		return nil
	}
	return map[string]string{"GUIDE-MINECRAFT-IA.md": `# Assistant IA dans Minecraft Java

La passerelle minecraft-ai relie le chat du serveur au modèle local défini dans compose.yaml. Le modèle utilisé pour concevoir cette infrastructure dans AutoDocker est distinct : ses clés API ne sont pas copiées dans le projet.

1. Lire et accepter vous-même l’EULA Minecraft. Le générateur laisse EULA=FALSE ; modifier cette valeur uniquement après acceptation.
2. Vérifier le matériel du moteur choisi. vLLM nécessite un GPU NVIDIA compatible et NVIDIA Container Toolkit. Le premier démarrage télécharge le modèle Hugging Face. Ollama télécharge également le modèle choisi au démarrage.
3. Démarrer avec docker compose up -d. Contrôler les journaux de minecraft-ai et attendre que le moteur serve son modèle.
4. Rejoindre le serveur Minecraft Java et écrire : @ia Bonjour, peux-tu m’expliquer comment fabriquer une torche ?
5. La réponse apparaît dans le chat. L’assistant peut expliquer et discuter ; il ne voit pas le monde, ne joue pas et n’exécute aucune commande demandée par un joueur ou par le modèle.

## Connexions

La passerelle lit uniquement les nouveaux messages des journaux Minecraft, appelle le moteur via son API interne puis affiche un message tellraw par RCON. Le mot de passe RCON est généré et partagé uniquement entre Minecraft et la passerelle. Le port 25575 ne doit pas être publié sur Internet. Le volume du jeu est monté en lecture seule dans la passerelle.

Le profil lit le format de chat standard Java, dont Vanilla et Paper. Des formats de logs modifiés par des plugins peuvent nécessiter une adaptation. Une seule question est traitée à la fois ; les messages pendant une réponse ne forment pas une file d’attente. La mémoire de conversation est courte, bornée et perdue au redémarrage de la passerelle.

## Accès Internet

Caddy route les interfaces HTTP, notamment Open WebUI. Minecraft utilise son port de jeu TCP séparé : le proxy HTTPS web ne le transforme pas en service HTTPS. Prévoir un domaine public, un certificat approuvé, les règles du routeur/pare-feu et les restrictions d’accès des joueurs. Un certificat privé localhost n’est pas un accès public prêt à l’emploi.

## Vérification avant livraison

Essayer un vrai échange @ia, vérifier le modèle effectivement chargé, le redémarrage du serveur et l’absence d’exposition de RCON. Les tests de génération et les simulations de protocole ne remplacent pas ce test sur le serveur cible.
`}
}
