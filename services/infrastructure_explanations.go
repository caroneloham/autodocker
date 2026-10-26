package services

import (
	"fmt"
	"strings"
)

func infrastructureIngressExplanation(payload map[string]interface{}, summary string, reasons map[string]string, warnings []string) (string, []string) {
	ingress, ok := payload["ingress"].(map[string]interface{})
	if !ok {
		return summary, warnings
	}
	proxy, _ := ingress["proxy"].(string)
	policy := "Porte d’entrée HTTP ; le domaine, le DNS et le certificat public restent à configurer pour Internet."
	if ingress["tls"] == "internal" {
		policy = "Porte d’entrée HTTPS avec certificat privé ; son autorité doit être approuvée par les clients. Aucun certificat public automatique n’est configuré."
	}
	if proxy != "" {
		reasons[proxy] = policy
	}
	claimsPublicTLS := func(text string) bool {
		w := infrastructureWords(text)
		return strings.Contains(w, "let s encrypt") || strings.Contains(w, "letsencrypt") || strings.Contains(w, "certificat public") || strings.Contains(w, "certificats publics") || strings.Contains(w, "https automatique")
	}
	if claimsPublicTLS(summary) {
		summary = fmt.Sprintf("Architecture de %d services préparée avec %s comme point d’entrée. %s", len(normalizeServiceList(payload["services"])), proxy, policy)
	}
	clean := []string{}
	for _, warning := range warnings {
		if !claimsPublicTLS(warning) {
			clean = append(clean, warning)
		}
	}
	clean = append(clean, policy)
	return summary, clean
}
