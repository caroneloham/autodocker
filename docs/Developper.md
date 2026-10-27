# Développer AutoDocker

[Retour au README](../README.md)


Il faut Go **1.26.10** ou une version compatible avec [`go.mod`](../go.mod). Les lancements locaux ci-dessous restent limités à `127.0.0.1` et créent un compte propriétaire sans mot de passe prédéfini. Leur état est placé dans `.local/state/`, ignoré par Git. N'exposez pas ce mode local sur le réseau.

```sh
go test ./...
go build -o autodocker .
```

Sous Linux, lancez `sh scripts/run-local.sh`. Sous Windows, lancez `scripts\run-local.cmd` après avoir installé Go. Ces scripts utilisent `go run .` et ouvrent l'interface sur `http://127.0.0.1:18743`. Dans **Mon compte**, vous pouvez ensuite choisir une IA locale ou une API et connecter Docker depuis l'interface. Aucun identifiant ni clé d'API n'est inclus dans ce dépôt.
