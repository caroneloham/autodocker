# Construire et héberger l’essai public

[Retour au README](../README.md)


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
