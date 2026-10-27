# Politique de sécurité

## Signaler une vulnérabilité en privé

Utilisez **[Report a vulnerability](https://github.com/caroneloham/autodocker/security/advisories/new)** dans l’onglet Security du dépôt. Ce canal transmet votre rapport de façon privée aux responsables du projet.

Ne publiez pas de preuve d’exploitation, de clé, de mot de passe ou de données privées dans une issue ou une pull request publique. Pour un bug sans conséquence de sécurité, vous pouvez utiliser les [issues](https://github.com/caroneloham/autodocker/issues).

## Informations utiles

- Version ou commit concerné, système utilisé et mode d’exécution.
- Étapes minimales de reproduction sur un environnement de test.
- Comportement observé, résultat attendu et impact concret.
- Exemple ou capture expurgé de tout secret, donnée de client ou adresse privée.

Le projet est en bêta. Indiquez si le problème se reproduit avec la dernière version publiée ; les signalements concernant une ancienne version restent utiles. Aucun délai de réponse ou de correction n’est garanti.

## Périmètre

Cette politique couvre l’application locale AutoDocker, ses lanceurs Windows/Linux, les fichiers générés et le serveur d’essai public `cmd/trial`.

Les éléments sensibles comprennent les comptes, sessions, clés des fournisseurs d’IA, projets et archives. L’application locale peut accéder à Docker ; l’essai public est limité à la génération, l’export et la visualisation.

## Propriétés de sécurité attendues

- Les opérations et fichiers d’un projet doivent respecter les autorisations de son utilisateur.
- Les clés, sessions et sauvegardes privées ne doivent pas être accessibles à un autre utilisateur ou divulguées dans une réponse ou un journal public.
- Les chemins et archives fournis en entrée doivent rester dans les répertoires autorisés.
- Les protections de session, d’origine et de CSRF doivent rester actives sur les opérations concernées.
- L’essai public ne doit pas donner accès au terminal, au moteur Docker ni aux fonctions d’administration de l’hôte.

Ces propriétés décrivent les limites à préserver ; elles ne constituent pas une attestation qu’un audit a validé tous les parcours. Un contournement reproductible avec un impact sur la confidentialité, l’intégrité ou la disponibilité doit être signalé, y compris lorsqu’il concerne une dépendance.

## Précautions et limites

Relisez les fichiers Compose et les scripts générés avant leur déploiement. La sortie d’une IA est une entrée à vérifier. Le mode local est conçu pour `127.0.0.1` et ne doit pas être exposé directement sur Internet.

Reproduisez les problèmes sur vos propres environnements de test, sans accéder aux données d’autrui ni perturber le service public. L’absence d’alerte automatique ne garantit pas l’absence de vulnérabilité.
