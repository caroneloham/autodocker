package services

import (
	"fmt"
	"strings"
)

// PrivateAIProjectFiles adds portable, credential-free operating instructions
// and a standard-library API client to the same atomic project transaction.
func PrivateAIProjectFiles(compose M) map[string]string {
	metadata, _ := compose["x-autodocker"].(M)
	if metadata == nil {
		return nil
	}
	ai, _ := metadata["ai"].(M)
	if ai == nil || ai["private_platform"] != true {
		return nil
	}
	base := "http://localhost:3001/api"
	if svcs, ok := compose["services"].(M); ok {
		if web, ok := svcs["open-webui"].(M); ok {
			for _, spec := range toStringList(web["ports"]) {
				host, inner, valid := infrastructurePublishedPort(spec)
				if valid && inner == 8080 {
					base = fmt.Sprintf("http://localhost:%d/api", host)
				}
			}
		}
	}
	ingress, _ := metadata["ingress"].(M)
	if ingress != nil {
		if routes, ok := ingress["routes"].([]M); ok {
			for _, route := range routes {
				if route["service"] == "open-webui" {
					base = fmt.Sprintf("%s://%s:%v/api", ingress["protocol"], route["host"], ingress["port"])
				}
			}
		}
	}
	model, _ := ai["served_model"].(string)
	guide := fmt.Sprintf(`# Votre plateforme IA privée

Chaîne générée : navigateur ou client API → Caddy → Open WebUI → vLLM.
Les documents utilisent Open WebUI → TEI (embeddings) + Qdrant (vecteurs).

## Démarrage

1. Vérifiez que le GPU NVIDIA et NVIDIA Container Toolkit fonctionnent dans Docker. Libérez assez de mémoire GPU avant le démarrage.
2. Lancez : docker compose up -d
3. Suivez le chargement : docker compose logs -f vllm tei
4. Vérifiez les états : docker compose ps

Modèle : %v. Contexte : %v tokens. Séquences simultanées : %v.
Le premier démarrage télécharge les images et modèles. Cette capacité limite les requêtes traitées en parallèle ; ce n’est pas un nombre garanti d’utilisateurs.

## Accès et comptes

Adresse API : %s
Adresse navigateur : %s
Le compte administrateur est initialisé au premier démarrage. Son adresse et son mot de passe généré sont dans l’environnement du service open-webui : WEBUI_ADMIN_EMAIL et WEBUI_ADMIN_PASSWORD. Le fichier Compose contient des secrets : gardez-le privé.
Changez le mot de passe initial dans Open WebUI. Créez ensuite les comptes et attribuez les droits depuis l’administration. Les inscriptions publiques sont désactivées. Chaque utilisateur autorisé peut utiliser sa propre clé API, limitée aux routes du chat, des modèles et des fichiers.

## HTTPS privé

Avec le mode HTTPS privé, Caddy conserve son autorité de certification dans le volume caddy_data. Exportez uniquement son certificat public :
docker compose cp caddy:/data/caddy/pki/authorities/local/root.crt ./autodocker-root.crt

Faites approuver ce certificat par les postes autorisés. Pour Python, indiquez son chemin dans AUTODOCKER_CA_FILE. Ne désactivez pas la vérification TLS. Pour une utilisation sur un autre ordinateur, configurez le domaine de base et le DNS vers le serveur avant le déploiement : les noms .localhost désignent le poste qui les ouvre.

## API texte et image

Le script ia-api.py utilise uniquement Python 3 et sa bibliothèque standard.
Variables à définir :
AUTODOCKER_API_BASE_URL=%s
AUTODOCKER_API_KEY=<clé personnelle créée dans Open WebUI>
AUTODOCKER_MODEL=%s
AUTODOCKER_CA_FILE=<chemin du certificat public Caddy si nécessaire>

Texte : python ia-api.py "Explique le rôle d’un reverse proxy"
Image : python ia-api.py "Décris cette image" --image photo.png
L’option image nécessite le profil vision et un modèle multimodal compatible.

## Documents et limites fonctionnelles

Importez les documents et créez les collections de connaissances dans Open WebUI, puis attribuez les droits aux utilisateurs et groupes concernés. Les connexions d’embeddings et de stockage vectoriel sont déjà déclarées.
Un PDF scanné peut nécessiter une extraction OCR supplémentaire. Les outils personnalisés de création de fichiers Word, Excel et PowerPoint de la conversation de référence ne sont pas installés par ce profil. Les modèles peuvent traiter le texte extrait ; une vraie fonction de création de fichiers nécessite son outil dédié.

## Persistance et arrêt

Arrêt temporaire : docker compose stop
Relance : docker compose start
Les volumes contiennent comptes, conversations, documents, index et caches. Conservez-les et sauvegardez-les avant une mise à jour. docker compose down sans -v conserve les volumes.
Les poids choisis, leur quantification et les appels d’outils doivent être compatibles avec le matériel et la version de vLLM. Le générateur vérifie le câblage ; il ne remplace pas un test de charge et un essai réel du modèle choisi.
`, ai["model"], ai["max_context"], ai["concurrency"], base, strings.TrimSuffix(base, "/api"), base, model)
	return map[string]string{"GUIDE-IA.md": guide, "ia-api.py": privateAIClient}
}

const privateAIClient = `#!/usr/bin/env python3
"""Client texte/image Open WebUI. Clé personnelle et certificat via l’environnement."""
import argparse
import base64
import json
import mimetypes
import os
from pathlib import Path
import ssl
import sys
import urllib.error
import urllib.parse
import urllib.request


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("prompt")
    parser.add_argument("--image", type=Path)
    args = parser.parse_args()
    base = os.environ.get("AUTODOCKER_API_BASE_URL", "").rstrip("/")
    key = os.environ.get("AUTODOCKER_API_KEY", "")
    parsed = urllib.parse.urlparse(base)
    if not key or parsed.scheme not in ("http", "https") or not parsed.hostname or parsed.username or parsed.password or parsed.query or parsed.fragment:
        parser.error("Définissez AUTODOCKER_API_BASE_URL et AUTODOCKER_API_KEY avec votre clé personnelle.")
    if parsed.scheme == "http" and parsed.hostname not in ("localhost", "127.0.0.1", "::1"):
        parser.error("L’accès distant à l’API utilise HTTPS.")
    content = args.prompt
    if args.image:
        mime = mimetypes.guess_type(args.image.name)[0]
        if mime not in ("image/png", "image/jpeg", "image/webp"):
            parser.error("Utilisez une image PNG, JPEG ou WebP.")
        if args.image.stat().st_size > 10 * 1024 * 1024:
            parser.error("L’image doit faire au maximum 10 Mio.")
        encoded = base64.b64encode(args.image.read_bytes()).decode("ascii")
        content = [{"type": "text", "text": args.prompt}, {"type": "image_url", "image_url": {"url": "data:" + mime + ";base64," + encoded}}]
    payload = {"model": os.environ.get("AUTODOCKER_MODEL", "autodocker-llm"), "messages": [{"role": "user", "content": content}], "stream": False}
    request = urllib.request.Request(base + "/chat/completions", data=json.dumps(payload).encode("utf-8"), headers={"Content-Type": "application/json", "Authorization": "Bearer " + key}, method="POST")
    ca = os.environ.get("AUTODOCKER_CA_FILE") or None
    context = ssl.create_default_context(cafile=ca)
    # Never forward a user's API credential through a redirect to another host.
    class NoRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, req, fp, code, msg, headers, newurl):
            return None
    opener = urllib.request.build_opener(NoRedirect, urllib.request.HTTPSHandler(context=context))
    with opener.open(request, timeout=180) as response:
        data = json.load(response)
    print(data["choices"][0]["message"]["content"])


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, KeyError, IndexError, urllib.error.URLError) as error:
        print("Appel API impossible : " + str(error), file=sys.stderr)
        sys.exit(1)
`
