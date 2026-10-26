package services

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"regexp"
	"strings"
	"unicode"
)

// ClientDeliveryConfig belongs to the generated project, never the generator's own identity.
type ClientDeliveryConfig struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Accent      string `json:"accent"`
	Logo        string `json:"logo,omitempty"`
	Support     string `json:"support,omitempty"`
}

type SupervisionConfig struct {
	GPU                bool   `json:"gpu"`
	DiskPath           string `json:"disk_path"`
	DiskWarning        int    `json:"disk_warning"`
	MinFreeGiB         int    `json:"min_free_gib"`
	TemperatureWarning int    `json:"temperature_warning"`
	BackupStatusFile   string `json:"backup_status_file"`
	BackupMaxHours     int    `json:"backup_max_hours"`
}

//go:embed client_assets/supervision.py
var supervisionScript string

//go:embed client_assets/portal.html
var clientPortalTemplate string

func plainClientText(s string, limit int) bool {
	return len([]rune(s)) <= limit && strings.IndexFunc(s, unicode.IsControl) < 0
}

func validateClientDelivery(p BuildComposePayload) error {
	if b := p.Client; b != nil {
		if strings.TrimSpace(b.Name) == "" || !plainClientText(b.Name, 80) || !plainClientText(b.Description, 240) || !plainClientText(b.Support, 160) {
			return fmt.Errorf("Identité client : renseignez un nom court et des textes sans caractères de contrôle")
		}
		if !regexp.MustCompile(`^#[0-9a-fA-F]{6}$`).MatchString(b.Accent) {
			return fmt.Errorf("Couleur client invalide : utilisez #RRGGBB")
		}
		if b.Logo != "" {
			parts := strings.SplitN(b.Logo, ",", 2)
			if len(parts) != 2 || (parts[0] != "data:image/png;base64" && parts[0] != "data:image/jpeg;base64") || len(parts[1]) > 700000 {
				return fmt.Errorf("Logo : utilisez un PNG ou JPEG de 512 Kio maximum")
			}
			data, err := base64.StdEncoding.DecodeString(parts[1])
			if err != nil || len(data) > 512*1024 {
				return fmt.Errorf("Logo invalide ou trop volumineux")
			}
			cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
			if err != nil || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 2048 || cfg.Height > 2048 || "data:image/"+format+";base64" != parts[0] {
				return fmt.Errorf("Logo : image PNG ou JPEG valide, 2048 × 2048 pixels maximum")
			}
		}
	}
	if m := p.Supervision; m != nil {
		if m.DiskPath == "" || !plainClientText(m.DiskPath, 500) || !plainClientText(m.BackupStatusFile, 500) {
			return fmt.Errorf("Supervision : indiquez un chemin de disque valide sur le serveur cible")
		}
		if m.DiskWarning < 50 || m.DiskWarning > 95 || m.MinFreeGiB < 1 || m.MinFreeGiB > 10000 || m.TemperatureWarning < 40 || m.TemperatureWarning > 95 || m.BackupMaxHours < 1 || m.BackupMaxHours > 8760 {
			return fmt.Errorf("Supervision : vérifiez les seuils de disque, température et ancienneté de sauvegarde")
		}
	}
	return nil
}

func applyClientDelivery(compose M, p BuildComposePayload) {
	if p.Client == nil && p.Supervision == nil {
		return
	}
	meta, _ := compose["x-autodocker"].(M)
	if meta == nil {
		meta = M{}
		compose["x-autodocker"] = meta
	}
	// Metadata declares which optional files the project transaction must save.
	meta["client_delivery"] = M{"version": 1, "client": p.Client, "supervision": p.Supervision}
}

type clientPortalLink struct{ Name, URL string }
type clientPortalData struct {
	Name, Description, Support string
	Accent                     template.CSS
	Logo                       template.URL
	Supervision                bool
	Links                      []clientPortalLink
}

// ClientDeliveryProjectFiles is also used for JSON-decoded metadata, so project
// archives and persisted Compose files use the same validated configuration.
func ClientDeliveryProjectFiles(compose M) map[string]string {
	meta, _ := compose["x-autodocker"].(M)
	if meta == nil || meta["client_delivery"] == nil {
		return nil
	}
	var cfg struct {
		Client      *ClientDeliveryConfig `json:"client"`
		Supervision *SupervisionConfig    `json:"supervision"`
	}
	encoded, err := json.Marshal(meta["client_delivery"])
	if err != nil || json.Unmarshal(encoded, &cfg) != nil || validateClientDelivery(BuildComposePayload{Client: cfg.Client, Supervision: cfg.Supervision}) != nil {
		return nil
	}
	view := clientPortalData{Name: "Votre infrastructure", Accent: template.CSS("#4f8cff"), Supervision: cfg.Supervision != nil}
	if b := cfg.Client; b != nil {
		view.Name = b.Name
		view.Description = b.Description
		view.Support = b.Support
		view.Accent = template.CSS(b.Accent)
		view.Logo = template.URL(b.Logo)
	}
	if ingress, ok := meta["ingress"].(M); ok {
		if routes, ok := ingress["routes"].([]M); ok {
			for _, r := range routes {
				view.Links = append(view.Links, clientPortalLink{Name: fmt.Sprint(r["service"]), URL: fmt.Sprintf("%s://%s:%v", ingress["protocol"], r["host"], ingress["port"])})
			}
		}
	}
	tmpl, err := template.New("portal").Parse(clientPortalTemplate)
	if err != nil {
		panic(err)
	} // A compile-time embedded template, never user code.
	var page bytes.Buffer
	if err := tmpl.Execute(&page, view); err != nil {
		return nil
	}
	files := map[string]string{"client.html": page.String(), "client.json": string(encoded), "GUIDE-CLIENT.md": clientDeliveryGuide}
	if cfg.Supervision != nil {
		settings, _ := json.MarshalIndent(cfg.Supervision, "", "  ")
		files["supervision.json"] = string(settings)
		files["supervision.py"] = supervisionScript
	}
	return files
}

const clientDeliveryGuide = `# Identité client et supervision

client.html est le portail de remise : nom, logo local, couleur, contact et liens vers les applications routées par le proxy. Ouvrez-le dans un navigateur pour consulter la remise. Les applications non routées ne sont pas présentées comme accessibles. Le domaine et le DNS choisis dans le générateur doivent correspondre au serveur cible.

Cette identité concerne le portail et le rapport AutoDocker. Elle ne remplace pas les marques ni les interfaces des logiciels tiers (dont Open WebUI). Les réglages spécifiques des applications restent propres à chaque client.

## Supervision sur le serveur cible

Si l’option est activée, copiez le projet sur le serveur Docker, puis exécutez Python 3.10 ou plus récent :

    python3 supervision.py --once

Ce contrôle produit supervision-status.json et retourne 0 si tous les contrôles activés sont corrects, 1 pour une alerte, 2 pour un état inconnu/non configuré. Il ne démarre, n’arrête et ne modifie aucun conteneur. Il n’exécute pas le système de sauvegarde.

Pour afficher le portail et actualiser les mesures toutes les 60 secondes :

    python3 supervision.py --serve

Ouvrez http://127.0.0.1:18798 sur le serveur. Le port peut être changé avec --port. Pour un serveur distant, utilisez un tunnel SSH vers ce port local. Le serveur écoute uniquement en boucle locale ; il ne publie pas les métriques sur le réseau. Arrêt avec Ctrl+C. Aucun service système n’est installé automatiquement. Pour un suivi permanent, exécutez cette commande sous le gestionnaire de services du client, avec son utilisateur et un chemin de projet absolu.

Exécutez le collecteur sur l’hôte, pas dans un conteneur : le disque et les GPU doivent être ceux du serveur. Sur Windows avec Docker Desktop, il mesure l’hôte Windows et le chemin Windows sélectionné ; il ne mesure pas le disque de la VM Docker. Sur Linux, Python doit pouvoir lire le chemin surveillé et exécuter nvidia-smi si le GPU est attendu.

## Mesures et états

supervision.json définit le disque, l’espace libre minimum, le taux d’utilisation, la température GPU et l’âge maximal des sauvegardes. Le GPU est lu via nvidia-smi, avec une limite de temps. Utilisation, VRAM et température sont affichées pour chaque GPU NVIDIA. Une VRAM élevée n’est pas à elle seule une panne ; la température est comparée au seuil. Une mesure inaccessible reste inconnue. Le rapport indique son heure de collecte et l’interface signale un rapport périmé.

## Brancher la sauvegarde du client

Indiquez dans backup_status_file le chemin du reçu JSON produit par le système de sauvegarde du client après la fin de son travail. Exemple de contrat :

    {"status":"ok","completed_at":"2026-10-18T03:15:00Z","verified":true}

Le système externe doit écrire ce reçu seulement après son contrôle de sauvegarde. status différent de ok ou verified différent de true signale une alerte ; une date trop ancienne signale aussi une alerte. Une date future ou illisible reste inconnue. Sans fichier configuré, l’état est « Non configuré », jamais « OK ». Ce reçu déclare le résultat du système externe : le collecteur ne vérifie pas lui-même le contenu des archives ni une restauration.

Le fichier de configuration et le reçu doivent être modifiables uniquement par les administrateurs autorisés. Les changements d’identité et de supervision se font dans les fichiers générés ; AutoDocker conserve les fichiers déjà présents lors d’une nouvelle génération pour ne pas écraser une adaptation client. Pour repartir d’une nouvelle configuration, utilisez un nouveau nom de projet.
`
