# Sécurité

N'utilisez pas une issue publique pour transmettre un exploit, une clé ou des données privées. Utilisez le signalement privé de vulnérabilité du dépôt GitHub lorsqu'il est disponible. S'il ne l'est pas, ouvrez une issue sans détails exploitables pour demander un canal privé.

Pour aider à reproduire le problème, indiquez la version concernée, le mode d'exécution (application locale ou essai), le comportement observé et les conditions minimales. Supprimez les identifiants, adresses privées et données de clients de tout exemple ou capture.

Les fichiers Compose et scripts générés doivent être examinés avant leur déploiement. Le mode local de premier lancement est conçu pour `127.0.0.1` ; ne l'exposez pas directement au réseau.
