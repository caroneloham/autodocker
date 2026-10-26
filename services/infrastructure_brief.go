package services

import (
	"regexp"
	"strings"
)

// Provider-independent checks. An explicit unknown is an answer, but becomes
// a documented assumption rather than a fictitious hardware guarantee.
func infrastructureBriefQuestions(prompt string) []string {
	w := infrastructureWords(prompt)
	questions := []string{}
	if len(strings.Fields(w)) < 4 && len(mentionedInfrastructureServices(prompt, infrastructureCatalogue())) == 0 {
		questions = append(questions, "Quel usage souhaitez-vous proposer, et à qui : site, fichiers, documents, chat IA, serveur de jeu… ?")
	}
	resources := regexp.MustCompile(`\b[0-9]+\s*(go|gio|gb|gib|mo|mb|v?cpu|coeurs?|cores?)\b|\b(rtx|tesla|a100|h100|sans gpu|cpu uniquement)\b`).MatchString(w)
	unknownResources := strings.Contains(w, "ressources : a dimensionner") || strings.Contains(w, "ressources a dimensionner") || strings.Contains(w, "materiel inconnu") || strings.Contains(w, "ressources inconnues")
	if !resources && !unknownResources {
		questions = append(questions, "Quelles ressources aura le serveur : RAM, CPU et GPU éventuel ? Si vous ne savez pas encore, indiquez « ressources à dimensionner ».")
	}
	access := regexp.MustCompile(`\b(internet|public|publique|prive|privee|local|localhost|interne|lan|vpn|intranet)\b`).MatchString(w)
	if !access {
		questions = append(questions, "L’accès sera-t-il local, réservé à votre réseau/VPN ou ouvert sur Internet ? Pour Internet, avez-vous déjà un domaine ?")
	}
	ai := regexp.MustCompile(`\b(ia|ai|llm|vllm|ollama|webui|rag|hermes|openclaw)\b`).MatchString(w)
	model := regexp.MustCompile(`\b(qwen|mistral|llama|gemma|phi|deepseek|tinyllama)[a-z0-9./:_-]*|(?:modele souhaite|modele)\s+\S`).MatchString(w)
	if ai && !model {
		questions = append(questions, "Quel modèle souhaitez-vous utiliser ? Vous pouvez répondre « modèle : à conseiller » pour une recommandation adaptée au matériel.")
	}
	if ai && strings.Contains(w, "vllm") && strings.Contains(w, "sans gpu") && !strings.Contains(w, "gpu distant") {
		questions = append(questions, "Le profil vLLM nécessite un GPU NVIDIA. Préférez-vous Ollama sur CPU, ou disposez-vous d’un GPU distant à préciser ?")
	}
	return questions
}
