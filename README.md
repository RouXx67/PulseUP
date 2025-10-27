# PulseUP

[![GitHub release](https://img.shields.io/github/v/release/RouXx67/PulseUP)](https://github.com/RouXx67/PulseUP/releases/latest)
[![License](https://img.shields.io/github/license/RouXx67/PulseUP)](LICENSE)

**Monitoring en temps réel pour Proxmox VE, Proxmox Mail Gateway, PBS, Docker et surveillance des mises à jour d'applications.**

PulseUP est un fork de [Pulse](https://github.com/RouXx67/PulseUP) qui intègre les fonctionnalités de [SelfUp](https://github.com/RouXx67/SelfUp) pour offrir une solution complète de monitoring d'infrastructure et de surveillance des mises à jour d'applications.

Surveillez votre infrastructure hybride Proxmox et Docker depuis un tableau de bord unique, tout en gardant un œil sur les mises à jour de vos applications. Recevez des alertes instantanées lorsque des nœuds tombent, des conteneurs dysfonctionnent, des sauvegardes échouent, le stockage se remplit, ou quand des mises à jour sont disponibles pour vos applications.

<img width="2872" height="1502" alt="image" src="https://github.com/user-attachments/assets/41ac125c-59e3-4bdc-bfd2-e300109aa1f7" />

## À propos de ce projet

PulseUP combine le meilleur de deux mondes :

- **Pulse** : Solution de monitoring robuste pour l'infrastructure Proxmox et Docker
- **SelfUp** : Système de surveillance des mises à jour d'applications

Cette intégration permet de centraliser la surveillance de votre infrastructure ET de vos applications dans une seule interface, avec un système d'alertes unifié.

## Fonctionnalités

### Monitoring d'Infrastructure (Pulse)

- **Auto-Discovery** : Trouve automatiquement les nœuds Proxmox sur votre réseau
- **Support Cluster** : Configurez un nœud, surveillez tout le cluster
- **Sécurité Entreprise** :
  - Identifiants chiffrés au repos, masqués dans les logs
  - Protection CSRF pour toutes les opérations
  - Limitation de débit (500 req/min général, 10 tentatives/min pour l'auth)
  - Verrouillage de compte après échecs de connexion
  - Gestion sécurisée des sessions avec cookies HttpOnly
  - Hachage bcrypt des mots de passe (coût 12)
  - Tokens API stockés de manière sécurisée
  - En-têtes de sécurité (CSP, X-Frame-Options, etc.)
  - Journalisation d'audit complète

- **Monitoring en temps réel** : VMs, conteneurs, nœuds, stockage
- **Alertes Intelligentes** : Email et webhooks (Discord, Slack, Telegram, Teams, ntfy.sh, Gotify)
  - Exemple : "VM 'webserver' est arrêtée sur le nœud 'pve1'"
  - Exemple : "Stockage 'local-lvm' à 85% de capacité"
  - Exemple : "VM 'database' est de nouveau en ligne"

- **Seuils Adaptatifs** : Niveaux de déclenchement/effacement basés sur l'hystérésis
- **Analyse de Timeline des Alertes** : Explorateur d'historique riche avec marqueurs
- **Conscience Ceph** : Santé Ceph, utilisation des pools, statut des démons
- **Vue unifiée des sauvegardes** : PBS, PVE et snapshots
- **Explorateur de Sauvegardes Interactif** : Graphiques et grilles avec pivots temporels
- **Analyse Proxmox Mail Gateway** : Volume de mails, tendances spam/virus
- **Monitoring Docker optionnel** : Via agent léger

### Surveillance des Mises à Jour (SelfUp)

- **Support Multi-Fournisseurs** : GitHub, Docker Hub, registres personnalisés
- **Vérification Automatisée** : Intervalles configurables pour la vérification des mises à jour
- **Tableau de Bord Visuel** : Statut des mises à jour et statistiques en temps réel
- **Intégration d'Alertes** : Notifications de mises à jour via le système d'alertes de Pulse
- **Suivi de Versions** : Historique des versions et comparaisons
- **Gestion Centralisée** : Ajout et configuration d'applications depuis l'interface

### Fonctionnalités Communes

- **Export/Import de Configuration** : Avec chiffrement et authentification
- **Mises à jour Automatiques** : Avec rollback sécurisé (opt-in)
- **Thèmes Sombre/Clair** : Design responsive
- **Construit avec Go** : Utilisation minimale des ressources
- **Interface Française** : Localisation complète

## Confidentialité

PulseUP respecte votre vie privée :

- Aucune télémétrie ou collecte d'analytics
- Aucune fonctionnalité de "phone-home"
- Aucun appel API externe (sauf webhooks configurés)
- Toutes les données restent sur votre serveur
- Open source - vérifiez par vous-même

Vos données d'infrastructure vous appartiennent exclusivement.

## Installation Rapide

### Installation

```bash
# Recommandé : Installateur officiel (détecte automatiquement Proxmox)
curl -fsSL https://raw.githubusercontent.com/RouXx67/PulseUP/main/install.sh | bash

# Revenir à une version précédente ? Passez le tag souhaité
curl -fsSL https://raw.githubusercontent.com/RouXx67/PulseUP/main/install.sh | bash -s -- --version v4.20.0

# Alternative : Docker
docker run -d -p 7655:7655 -v pulse_data:/data rouxxx67/pulseup:latest

# Test : Installation depuis la branche main (pour tester les derniers correctifs)
curl -fsSL https://raw.githubusercontent.com/RouXx67/PulseUP/main/install.sh | bash -s -- --source

# Alternative : Kubernetes (Helm)
helm registry login ghcr.io
helm install pulseup oci://ghcr.io/rouxxx67/pulseup-chart \
  --version $(curl -fsSL https://raw.githubusercontent.com/RouXx67/PulseUP/main/VERSION) \
  --namespace pulseup \
  --create-namespace
```

**Utilisateurs Proxmox** : L'installateur détecte les hôtes PVE et crée automatiquement un conteneur LXC optimisé. Choisissez le mode Rapide pour une installation en une minute.

[Options d'installation avancées →](docs/INSTALL.md)

### Mise à jour

- **Mises à jour Automatiques** : Activez lors de l'installation ou via l'interface Paramètres
- **Installation Standard** : Relancez l'installateur
- **Docker** : `docker pull rouxxx67/pulseup:latest` puis recréez le conteneur

### Configuration Initiale

#### Option A : Configuration Interactive (Interface)

1. Ouvrez `http://<votre-serveur>:7655`
2. Complétez la configuration de sécurité obligatoire (première fois uniquement)
3. Créez votre nom d'utilisateur et mot de passe administrateur
4. Utilisez Paramètres → Sécurité → Tokens API pour créer des tokens dédiés

#### Option B : Configuration Automatisée (Sans Interface)

Pour les déploiements automatisés, configurez l'authentification via variables d'environnement.

## Surveillance des Mises à Jour d'Applications (SelfUp)

### Démarrage Rapide

1. **Accédez à l'onglet SelfUp** dans l'interface PulseUP
2. **Ajoutez vos applications** :
   - Cliquez sur "Ajouter une App"
   - Sélectionnez le fournisseur (GitHub, Docker Hub, etc.)
   - Configurez les paramètres de surveillance
3. **Configurez les alertes** pour recevoir des notifications de mises à jour
4. **Surveillez le tableau de bord** pour voir le statut des mises à jour

### Fournisseurs Supportés

- **GitHub** : Releases et tags
- **Docker Hub** : Images et tags
- **Registres Personnalisés** : API compatibles
- **Plus à venir** : GitLab, npm, PyPI, etc.

### Fonctionnalités du Tableau de Bord

- **Vue d'ensemble** : Statistiques globales des mises à jour
- **Liste des Applications** : Statut détaillé par application
- **Historique** : Suivi des versions et changements
- **Actions Rapides** : Vérification manuelle, configuration

## Documentation

- [Guide d'Installation](docs/INSTALL.md)
- [Configuration](docs/CONFIGURATION.md)
- [Monitoring Docker](docs/DOCKER_MONITORING.md)
- [Configuration SelfUp](docs/SELFUP.md)
- [API](docs/API.md)
- [Sécurité](docs/SECURITY.md)
- [Dépannage](docs/TROUBLESHOOTING.md)
- [FAQ](docs/FAQ.md)

## Développement

PulseUP est construit avec :
- **Backend** : Go avec Gin framework
- **Frontend** : SolidJS avec TypeScript
- **Base de données** : SQLite (intégrée)
- **Containerisation** : Docker et LXC

[Guide de développement →](docs/development/README.md)

## Contribution

Les contributions sont les bienvenues ! Veuillez consulter notre [guide de contribution](CONTRIBUTING.md) pour commencer.

## Support

- **Issues** : [GitHub Issues](https://github.com/RouXx67/PulseUP/issues)
- **Discussions** : [GitHub Discussions](https://github.com/RouXx67/PulseUP/discussions)
- **Documentation** : [Wiki](https://github.com/RouXx67/PulseUP/wiki)

## Licence

Ce projet est sous licence [MIT](LICENSE).

## Remerciements

- **Pulse** : Merci à [rcourtman](https://github.com/rcourtman) pour le projet Pulse original
- **SelfUp** : Projet de surveillance des mises à jour intégré
- **Communauté** : Tous les contributeurs et utilisateurs qui rendent ce projet possible

---

**PulseUP** - Monitoring d'infrastructure et surveillance des mises à jour, unifiés.
