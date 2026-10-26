# AutoDocker

<img src="docs/assets/logo.png" alt="Logo AutoDocker" width="180">

AutoDocker aide à créer une infrastructure Docker à partir d'un formulaire ou d'une demande en langage naturel. L'application locale peut aussi se connecter à Docker ou Docker Desktop et à une IA locale (Ollama, LM Studio, vLLM) ou à une API, depuis l'interface graphique. La génération manuelle reste utilisable sans fournisseur d'IA ni moteur Docker.

**Essayer en ligne :** [autodocker.eu/essai](https://autodocker.eu/essai/) — génération, export et visualisation d'un fichier Compose. L'essai public dispose d'une IA réelle avec quota. Il n'expose pas les fonctions locales de gestion de l'hôte Docker.

**Installer sur son ordinateur :** [télécharger la dernière version pour Windows ou Linux](https://github.com/caroneloham/autodocker/releases), extraire l'archive complète et lancer le script fourni. Le premier lancement ouvre la création du compte propriétaire. Les comptes, projets et réglages restent dans le dossier extrait ; gardez ce dossier pour conserver vos données. Docker est nécessaire pour inspecter ou gérer des conteneurs, mais pas pour générer des fichiers.

![Interface AutoDocker](docs/assets/demo.png)

## Construire depuis les sources

Il faut Go **1.26.10** ou une version compatible avec [`go.mod`](go.mod). Les lancements locaux ci-dessous restent limités à `127.0.0.1` et créent un compte propriétaire sans mot de passe prédéfini. Leur état est placé dans `.local/state/`, ignoré par Git. N'exposez pas ce mode local sur le réseau.

```sh
go test ./...
go build -o autodocker .
```

Sous Linux, lancez `sh scripts/run-local.sh`. Sous Windows, lancez `scripts\run-local.cmd` après avoir installé Go. Ces scripts utilisent `go run .` et ouvrent l'interface sur `http://127.0.0.1:18743`. Dans **Mon compte**, vous pouvez ensuite choisir une IA locale ou une API et connecter Docker depuis l'interface. Aucun identifiant ni clé d'API n'est inclus dans ce dépôt.

## Essai public et visualiseur Docker Tracer

`cmd/trial` est un serveur distinct : génération et export sans compte, IA soumise à quota, comptes réservés à l'essai. Il ne déclare aucune route de terminal, de déploiement Docker ou de gestion de l'hôte. Son visualiseur de fichier Compose est construit à partir du projet frère [Docker Tracer](https://github.com/caroneloham/docker-tracer). **Le code source de Docker Tracer n'est pas copié ici.**

La construction de cette interface demande aussi Node.js et npm.

Pour préparer l'interface complète de l'essai dans un clone local :

```sh
git clone https://github.com/caroneloham/autodocker.git
git clone https://github.com/caroneloham/docker-tracer.git
cd docker-tracer
npm ci
npm run build:browser
cd ../autodocker
python scripts/build-trial-ui.py
go build -o autodocker-trial ./cmd/trial
```

Placez les deux dépôts côte à côte. `npm run build:browser` construit le visualiseur depuis les sources de Docker Tracer ; le script Python l'assemble dans `cmd/trial/ui/tracer/` avec l'interface de l'essai. Ce répertoire généré n'est pas versionné ; il faut reconstruire l'interface après un clone avant d'utiliser `/essai/tracer/`. Le serveur d'essai sert les fichiers de `cmd/trial/ui/` depuis son répertoire de travail, ou depuis le chemin défini par `AUTODOCKER_TRIAL_ASSETS`.

Pour un essai **strictement local**, fournissez ces variables d'environnement au processus `go run ./cmd/trial` :

| Variable | Usage |
| --- | --- |
| `AUTODOCKER_TRIAL_LOCAL=1` | Cookie de session adapté à HTTP pour un lancement strictement local ; gardez l'adresse d'écoute par défaut. |
| `AUTODOCKER_TRIAL_QUOTA_FILE` | Chemin d'un fichier JSON de quotas dans un dossier privé et inscriptible. |
| `AUTODOCKER_TRIAL_ACCOUNTS_FILE` | Chemin d'un fichier JSON de comptes dans un dossier privé et inscriptible. |
| `GEMINI_API_KEY` ou `OPENROUTER_API_KEY` | Clé du fournisseur d'IA, fournie hors dépôt. |

Le serveur est alors accessible sur `http://127.0.0.1:18082/essai/`. Pour l'hébergement public, le mode local doit être désactivé : fournir une clé de cookie stable de 32 octets bruts via `AUTODOCKER_TRIAL_COOKIE_KEY_FILE`, limiter l'écoute au mandataire HTTPS de confiance et conserver les fichiers d'état hors du dépôt. `AUTODOCKER_TRIAL_ADDR` règle l'adresse d'écoute. Le fichier Compose chargé dans le visualiseur est interprété dans le navigateur ; la carte montre la déclaration, sans vérifier l'état réel d'un hôte Docker.

Les limites actuelles de l'IA sont **2 demandes par cookie invité et par jour UTC**, avec un plafond additionnel de **6 par IP et par jour**, puis **5 par compte et par jour** ; le plafond commun est **40 demandes par jour**. Une demande est exécutée à la fois et une autre peut patienter. Les autres limites de requêtes et d'inscription se trouvent dans `cmd/trial`. Les quotas limitent les abus ordinaires, sans constituer une protection absolue. Un compte d'essai n'a actuellement ni vérification d'adresse e-mail ni récupération de mot de passe.

## Portée et licence

Les fichiers générés doivent être relus avant un déploiement réel. Une prévisualisation ne prouve pas qu'une infrastructure est adaptée ou sûre en production. L'IA dépend d'un fournisseur configuré par l'utilisateur ou l'exploitant.

Ce dépôt est publié sous la [licence AutoDocker et Docker Tracer 1.0](LICENSE), **à code source accessible**. L'usage en entreprise, les modifications et la vente des infrastructures ou prestations produites sont permis selon ses conditions. La vente du logiciel ou d'un accès SaaS payant est interdite. Ce n'est **pas** une licence open source approuvée par l'OSI. Les composants tiers conservent leurs propres licences et marques ; voir [les notices](docs/attributions/Logos-et-marques.txt).

Les contributions sont décrites dans [CONTRIBUTING.md](CONTRIBUTING.md), et le signalement responsable de vulnérabilités dans [SECURITY.md](SECURITY.md).
