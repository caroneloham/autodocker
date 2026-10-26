package services

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

type InfrastructureDesignCheck struct {
	Title  string `json:"title"`
	Detail string `json:"detail"`
}

// Recognise an explicit document-search need without treating every file server
// or mention of PDF as an instruction to install a RAG stack.
func infrastructureDocumentSearchRequested(prompt string) bool {
	words := infrastructureWords(prompt)
	if regexp.MustCompile(`\b(?:sans|pas de|without|ni|aucun|aucune) (?:rag|recherche documentaire|indexation)\b`).MatchString(words) {
		return false
	}
	if regexp.MustCompile(`\brag\b|\brecherche (?:semantique|documentaire)\b`).MatchString(words) {
		return true
	}
	return regexp.MustCompile(`\b(?:poser des questions|interroger|questionner|discuter|rechercher|recherche dans)\b.{0,100}\b(?:documents?|pdf|fichiers?)\b`).MatchString(words)
}

// These explanations are derived from validated configuration, not a second
// model's opinion. They describe planned wiring and explicit remaining work.
func infrastructureDesignReview(prompt string, payload map[string]interface{}) ([]InfrastructureDesignCheck, []string) {
	services := normalizeServiceList(payload["services"])
	checks := []InfrastructureDesignCheck{}
	steps := []string{"Tester le démarrage et la persistance sur le serveur cible ; la proposition n’exécute aucun conteneur."}
	mentioned := mentionedInfrastructureServices(prompt, infrastructureCatalogue())
	wanted, excluded := []string{}, []string{}
	for key, present := range mentioned {
		if present {
			wanted = append(wanted, key)
		} else {
			excluded = append(excluded, key)
		}
	}
	sort.Strings(wanted)
	sort.Strings(excluded)
	if len(wanted) > 0 {
		checks = append(checks, InfrastructureDesignCheck{Title: "Services demandés conservés", Detail: strings.Join(wanted, ", ")})
	}
	if len(excluded) > 0 {
		checks = append(checks, InfrastructureDesignCheck{Title: "Exclusions respectées", Detail: strings.Join(excluded, ", ")})
	}
	checks = append(checks, InfrastructureDesignCheck{Title: "Structure contrôlée", Detail: fmt.Sprintf("%d services ; dépendances, réseaux, ports et volumes vérifiés par le générateur.", len(services))})
	keys := map[string]bool{}
	custom := false
	for _, s := range services {
		key, _ := s["key"].(string)
		keys[key] = true
		if infrastructureCatalogue()[key] == "" {
			custom = true
		}
	}
	if (keys["open-webui"] || keys["anythingllm"]) && keys["tei"] && keys["qdrant"] {
		checks = append(checks, InfrastructureDesignCheck{Title: "Recherche documentaire préparée", Detail: "Interface, embeddings TEI et stockage Qdrant reliés par les profils intégrés."})
		steps = append(steps, "Importer les documents, attribuer les droits et vérifier une réponse avec citation sur un document connu.")
	}
	if keys["vllm"] {
		if raw, err := json.Marshal(payload); err == nil {
			var p BuildComposePayload
			if json.Unmarshal(raw, &p) == nil {
				c, _ := professionalAIConfig(p)
				checks = append(checks, InfrastructureDesignCheck{Title: "Modèle IA défini", Detail: "vLLM chargera " + c.Model + " ; le premier démarrage téléchargera les poids."})
			}
		}
		steps = append(steps, "Vérifier la mémoire GPU, le modèle choisi et sa compatibilité avec vLLM avant déploiement.")
	}
	if keys["minecraft-ai"] {
		checks = append(checks, InfrastructureDesignCheck{Title: "Chat Minecraft relié à l’IA", Detail: "La passerelle lit les messages @ia, appelle le modèle choisi et renvoie sa réponse dans le chat par RCON interne. Elle n’exécute aucune action de jeu."})
		steps = append(steps, "Accepter vous-même l’EULA Minecraft, démarrer Java Edition puis essayer « @ia Bonjour ». Le protocole de jeu utilise son port TCP, séparé du HTTPS web ; ne pas exposer RCON.")
	}
	if strings.Contains(infrastructureWords(prompt), "internet") {
		steps = append(steps, "Accès Internet : renseigner le domaine public, installer un certificat approuvé et configurer les ports du routeur/pare-feu. Le HTTPS privé localhost ne constitue pas un accès Internet prêt à l’emploi.")
	}
	if _, ok := payload["ingress"]; ok {
		steps = append(steps, "Configurer le domaine, le DNS et la confiance du certificat pour les postes autorisés.")
		if strings.Contains(infrastructureWords(prompt), "https") {
			steps = append(steps, "Le HTTPS géré utilise un certificat interne : pour Internet, remplacer/configurer la chaîne de confiance et les ports publics sur le serveur cible. Aucun certificat public automatiquement approuvé n’est promis.")
		}
	}
	if custom {
		steps = append(steps, "Vérifier les images et les réglages applicatifs des services hors catalogue ; leur intégration n’est pas certifiée par un profil intégré.")
	}
	if keys["grafana"] || keys["loki"] {
		steps = append(steps, "Tester les sources Grafana préconfigurées vers les services sélectionnés ; configurer les collecteurs et les cibles des machines externes.")
	}
	if keys["wordpress"] && keys["nextcloud"] && keys["mariadb"] {
		checks = append(checks, InfrastructureDesignCheck{Title: "Comptes SQL séparés", Detail: "L’archive provisionne une base et un compte dédiés par application sur un volume neuf ; une migration reste nécessaire pour une base existante."})
	}
	if keys["gitea"] && strings.Contains(infrastructureWords(prompt), "ssh") {
		steps = append(steps, "Vérifier l’accès SSH Git : le profil Gitea ne publie pas de port SSH par défaut ; ajouter sa publication si elle n’apparaît pas dans le Compose final.")
	}
	return checks, steps
}
