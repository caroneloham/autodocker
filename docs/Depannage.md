# Dépannage AutoDocker

Avant de modifier quoi que ce soit, relevez le message exact et vérifiez que vous utilisez la [dernière version publiée](https://github.com/caroneloham/autodocker/releases). Gardez vos projets et réglages ; supprimer les données de l’application n’est pas une étape de dépannage.

## Trouver le bon cas

| Symptôme | Où regarder |
| :--- | :--- |
| Le programme ne démarre pas ou le navigateur ne s’ouvre pas | [Lancement](#lapplication-ne-démarre-pas) |
| Message « Première ouverture » ou erreur 403 après le mot de passe | [Premier compte et session](#première-ouverture-ou-erreur-403) |
| Page inaccessible ou port déjà utilisé | [Port local](#page-inaccessible-ou-port-occupé) |
| Docker n’est pas connecté | [Connexion Docker](#docker-nest-pas-connecté) |
| L’IA ne répond pas ou le modèle est introuvable | [Connexion IA](#lia-ne-répond-pas) |
| Quota atteint, file occupée ou page en ligne indisponible | [Essai public](#erreurs-dans-lessai-public) |

## L’application ne démarre pas

1. Extrayez **toute** l’archive dans un dossier où votre utilisateur peut écrire, avant de lancer le programme. Ne lancez pas un fichier depuis l’intérieur du ZIP.
2. Utilisez `Lancer AutoDocker.cmd` sous Windows, ou `./lancer-autodocker.sh` sous Linux. Le paquet est prévu pour une machine **x64** ; un paquet Linux ne se lance pas directement sous Windows.
3. Sous Windows, gardez ouverte la fenêtre du lanceur et relevez le dernier message. Si `autodocker.exe` manque, réextrayez une archive téléchargée depuis les releases officielles.
4. Si Windows ou un poste géré bloque le fichier, consultez le message et l’historique de protection, vérifiez la provenance et le SHA-256 avec le fichier `SHA256SUMS.txt` de la release, puis demandez l’aide de votre administrateur. Ne désactivez pas la protection du poste pour poursuivre.

Sous Linux, si le script répond `Permission denied`, ouvrez un terminal dans le dossier extrait et lancez `sh ./lancer-autodocker.sh`. Si le message concerne le programme lui-même, vérifiez l’extraction et l’architecture. Pour une archive officielle extraite dans votre dossier personnel, vous pouvez rétablir uniquement les droits d’exécution des deux fichiers concernés :

```sh
chmod u+x ./lancer-autodocker.sh ./autodocker
```

Si le disque interdit l’exécution, extrayez le paquet dans un dossier personnel sur un système de fichiers qui l’autorise.

Pour un lancement **depuis les sources**, `go` doit être installé à la version attendue dans `go.mod`. Les scripts sont alors `scripts/run-local.cmd` ou `sh scripts/run-local.sh` ; voir le [guide de développement](https://github.com/caroneloham/autodocker/blob/main/docs/Developper.md).

## Première ouverture ou erreur 403

Le premier compte doit être créé depuis l’onglet ouvert par le lanceur. Une ancienne adresse copiée, un autre navigateur ou un autre port peut ne pas posséder l’autorisation initiale.

1. Fermez la fenêtre serveur de **cette installation** d’AutoDocker.
2. Relancez le script dans le **même dossier extrait**.
3. Utilisez l’onglet ouvert automatiquement pour choisir le mot de passe. Gardez la même adresse et le même navigateur jusqu’à la fin.
4. Vérifiez que le navigateur accepte les cookies nécessaires pour cette adresse locale. Si vous souhaitez utiliser Brave, définissez-le comme navigateur par défaut avant la relance.
5. Si le formulaire renvoie encore 403 après validation, relevez la version et essayez la dernière archive publiée dans un dossier séparé, en conservant l’ancienne installation et ses données.

Ne désactivez pas les vérifications de session, d’origine ou de CSRF. Ne supprimez pas `auth.db`, `secrets/` ou `projects/` pour faire disparaître le message. Un problème de mot de passe oublié doit être signalé sans joindre la base de comptes ni transmettre le mot de passe.

## Page inaccessible ou port occupé

Le port local habituel est **18743**. La fenêtre serveur doit rester ouverte pendant l’utilisation. Utilisez l’adresse locale ouverte par le lanceur ; une page `/setup` laissée ouverte depuis un essai précédent peut viser une instance arrêtée.

Si le lanceur indique que le port est déjà utilisé, identifiez d’abord le programme concerné :

**PowerShell Windows, lecture seule :**

```powershell
Get-NetTCPConnection -LocalPort 18743 -State Listen -ErrorAction SilentlyContinue |
  Select-Object LocalAddress, LocalPort, OwningProcess
```

**Linux, lecture seule, si `ss` est disponible :**

```sh
ss -ltnp 'sport = :18743'
```

Sous Windows, `OwningProcess` donne le PID : retrouvez-le dans l’onglet **Détails** du Gestionnaire des tâches pour identifier le programme.

Fermez normalement l’ancienne instance d’AutoDocker si vous l’avez identifiée, puis relancez le bon dossier. Ne terminez pas un processus inconnu et ne publiez pas ce port sur Internet.

## Docker n’est pas connecté

1. Démarrez Docker Engine ou Docker Desktop. Sous Windows, utilisez le moteur de conteneurs Linux prévu par le projet.
2. Dans un terminal du même utilisateur, lancez `docker info` et `docker compose version` si le client Docker est installé. Une erreur ici indique un problème de moteur, de contexte, de client ou de droits à résoudre avant la connexion dans AutoDocker.
3. Dans **Mon compte**, sélectionnez la connexion Docker locale adaptée, puis vérifiez à nouveau son état.
4. Sous Linux, si l’accès au socket est refusé, faites vérifier les droits de votre utilisateur par l’administrateur. Ne rendez pas le socket Docker accessible à tout le monde.

La génération manuelle de fichiers reste utilisable même si Docker est arrêté.

## L’IA ne répond pas

Les réglages ci-dessous concernent **l’application locale**, dans **Mon compte**. Les adresses indiquées sont les valeurs habituelles ; utilisez le port réellement affiché par votre fournisseur.

| Fournisseur | Vérifier |
| :--- | :--- |
| Ollama | Service démarré, modèle installé et adresse locale, habituellement `http://127.0.0.1:11434`. |
| LM Studio | Modèle chargé, serveur local démarré et nom de modèle correct, habituellement `http://127.0.0.1:1234`. |
| vLLM | Serveur lancé, modèle servi et identifiant correct, habituellement `http://127.0.0.1:8000`. |
| API Gemini ou OpenRouter | Bon fournisseur sélectionné, clé valide, identifiant de modèle autorisé et quota disponible chez le fournisseur. |

Les connexions des fournisseurs locaux attendent `localhost` ou une adresse de boucle locale. Une adresse distante ne convient pas à ce réglage. En cas de délai dépassé, regardez aussi les journaux du fournisseur : un modèle en cours de chargement ou trop lourd peut ne pas répondre à temps. Évitez d’envoyer la même demande en boucle.

## Erreurs dans l’essai public

- **Quota personnel atteint / HTTP 429 :** le message précise la limite invitée, celle du compte, de la connexion ou de l’ensemble de l’essai. Attendez la réinitialisation du jour UTC. Un compte gratuit permet cinq demandes par jour selon les limites documentées ; il ne retire pas le plafond commun du service.
- **File occupée :** laissez terminer la demande déjà envoyée et réessayez plus tard si le message le demande. Plusieurs clics successifs n’accélèrent pas le traitement.
- **Demande trop longue ou invalide :** raccourcissez-la selon le message affiché et précisez les services souhaités.
- **Page blanche, 404 ou service indisponible :** rechargez une fois la [démo officielle](https://autodocker.eu/essai/), puis signalez l’heure, l’URL sans jeton et le message si le problème persiste. Ne saisissez pas à nouveau votre mot de passe dans une page inhabituelle.
- **Atelier Docker Tracer absent après une construction depuis les sources :** reconstruisez les fichiers statiques selon [le guide d’assemblage](https://github.com/caroneloham/autodocker/blob/main/docs/Essai-public.md). Ce cas doit être corrigé par la personne qui héberge l’essai.

## Demander de l’aide

Si le problème persiste, ouvrez une [issue](https://github.com/caroneloham/autodocker/issues) avec ce modèle :

```text
Version ou tag :
Windows/Linux et architecture :
Navigateur et version :
Application locale / atelier en ligne / lancement depuis les sources :
Étapes pour reproduire :
Résultat attendu :
Message exact et résultat observé :
```

Ajoutez seulement l’extrait de journal utile ou un Compose minimal reproduisant le problème. Masquez les clés d’API, mots de passe, adresses privées, données de clients et URLs contenant un jeton d’accès. N’envoyez pas votre dossier d’état complet, votre base de comptes ni vos clés SSH. Pour une vulnérabilité, utilisez le **[signalement privé](https://github.com/caroneloham/autodocker/security/advisories/new)** indiqué dans la [politique de sécurité](https://github.com/caroneloham/autodocker/blob/main/SECURITY.md).
