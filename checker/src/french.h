/*
 * Software Name : EURO/ROADEF Challenge 2026 checker
 * SPDX-FileCopyrightText: Copyright (c) Orange SA
 * SPDX-License-Identifier: MIT
 *
 * This software is distributed under the MIT licence,
 * see the "LICENSE" file for more details or https://opensource.org/license/MIT
 *
 * Authors: see CONTRIBUTORS.md
 * Software description: checker for the EURO/ROADEF Challenge 2026
 */

/**
 * @file french.h
 * @brief French language header file for the EURO/ROADEF Challenge 2026 checker.
 *
 * @author Morgan Chopin (morgan.chopin@orange.com)
 */

#ifndef _CHECKER_FRENCH_H_
#define _CHECKER_FRENCH_H_

// Icons for status messages
#define ICON_CROSS "❌"
#define ICON_CHECK "✅"

// Help messages
#define HELP_MSG_0 "Fichier JSON décrivant la topologie du réseau."
#define HELP_MSG_1 "Fichier JSON décrivant la matrice de trafic."
#define HELP_MSG_2 "Fichier JSON décrivant le scénario d'interventions."
#define HELP_MSG_3 "Fichier JSON décrivant les chemins SR associés aux demandes."
#define HELP_MSG_4 "Activer les logs de sortie"
#define HELP_MSG_5 "Préfixe du nom d'instance (alternative à la spécification de fichiers individuels). Charge automatiquement <nom>-net.json, <nom>-tm.json, <nom>-scenario.json et <nom>-srpaths.json."
#define HELP_MSG_6 "Activer l'affichage formaté avec graphiques"
#define HELP_MSG_7 "Définit le nombre maximum de décimales pour les valeurs à virgule flottante (par défaut : 12)."

// Error messages
#define ERR_MSG_0 ICON_CROSS " Impossible de lire les fichiers d'entrée."
#define ERR_MSG_1 ICON_CROSS " Le réseau d'entrée n'est PAS bidirectionnel."
#define ERR_MSG_2 ICON_CROSS " Le réseau d'entrée est vide ou n'a pas d'arcs."
#define ERR_MSG_3 ICON_CROSS " Le graphe de demandes est vide ou n'a pas de demandes."
#define ERR_MSG_4 ICON_CROSS " La contrainte de budget est violée (coût au moins {} > budget : {}) au pas de temps {}."
#define ERR_MSG_5 ICON_CROSS " Un budget dans la liste des budgets n'est pas un objet valide."
#define ERR_MSG_6 ICON_CROSS " {} : Format de demande incorrect"
#define ERR_MSG_7 ICON_CROSS " {} : La taille du tableau de valeurs de demande ne correspond pas au nombre de créneaux horaires"
#define ERR_MSG_8 ICON_CROSS " {} : 'num_time_slots' doit être un entier positif."
#define ERR_MSG_9 ICON_CROSS " {} : 'max_segments' doit être un entier non négatif."
#define ERR_MSG_10 ICON_CROSS " {} : La valeur du budget doit être un entier non négatif."
#define ERR_MSG_11 ICON_CROSS " {} : Une intervention dans la liste des interventions n'est pas un objet valide."
#define ERR_MSG_12 ICON_CROSS " {} : Un chemin SR dans la liste des chemins SR n'est pas un objet valide."
#define ERR_MSG_13 ICON_CROSS " {} : Format de chemin SR incorrect"
#define ERR_MSG_14 ICON_CROSS " {} : Le chemin SR a plus de segments ({}) que le maximum autorisé ({})"
#define ERR_MSG_15 ICON_CROSS " Erreur d'analyse : Le document n'est pas un fichier JSON valide. Code d'erreur : {} (décalage : {})"
#define ERR_MSG_16 ICON_CROSS " Le document est invalide, il doit être un objet"
#define ERR_MSG_17 ICON_CROSS " L'attribut {} est manquant ou n'est pas un entier"
#define ERR_MSG_18 ICON_CROSS " L'attribut {} est manquant ou n'est pas un tableau"
#define ERR_MSG_19 ICON_CROSS " {} '{}' est hors limites [{},{})"
#define ERR_MSG_20 ICON_CROSS " Chaque demande doit être assignée à un chemin SR"
#define ERR_MSG_21 ICON_CROSS " Le nœud source de la demande {} -> {} est introuvable dans le réseau"
#define ERR_MSG_22 ICON_CROSS " Le nœud cible de la demande {} -> {} est introuvable dans le réseau"
#define ERR_MSG_23 ICON_CROSS " Fichier(s) requis manquant(s) :"
#define ERR_MSG_24 "  --net (fichier de topologie réseau)"
#define ERR_MSG_25 "  --tm (fichier de matrice de trafic)"
#define ERR_MSG_26 "  --scenario (fichier de scénario)"
#define ERR_MSG_27 "  --srpaths (fichier de solution des chemins SR)"
#define ERR_MSG_28 " Veuillez fournir tous les fichiers requis ou utiliser --instance <nom>."
#define ERR_MSG_29 ICON_CROSS " Le point de passage {} est introuvable dans le réseau"
#define ERR_MSG_30 ICON_CROSS " Impossible de charger le fichier de schéma réseau '{}'"
#define ERR_MSG_31 ICON_CROSS " Le fichier de schéma réseau '{}' est introuvable ou n'est pas un JSON valide"
#define ERR_MSG_32 ICON_CROSS " La validation JSON du réseau a échoué. Schéma invalide à : {}"
#define ERR_MSG_33 ICON_CROSS " La validation JSON du réseau a échoué. Mot-clé invalide : {}"
#define ERR_MSG_34 ICON_CROSS " La validation JSON du réseau a échoué. Document invalide à : {}"
#define ERR_MSG_35 ICON_CROSS " JSON réseau : ID de nœud dupliqué {} trouvé"
#define ERR_MSG_36 ICON_CROSS " JSON réseau : Le lien #{} référence un ID de nœud non défini (from={}, to={})"
#define ERR_MSG_37 ICON_CROSS " JSON réseau : Lien dupliqué trouvé (from={}, to={}). Multigraphe non autorisé."
#define ERR_MSG_38 ICON_CROSS " Le fichier de schéma de matrice de trafic '{}' est introuvable ou n'est pas un JSON valide"
#define ERR_MSG_39 ICON_CROSS " La validation JSON de la matrice de trafic a échoué. Schéma invalide à : {}"
#define ERR_MSG_40 ICON_CROSS " La validation JSON de la matrice de trafic a échoué. Mot-clé invalide : {}"
#define ERR_MSG_41 ICON_CROSS " La validation JSON de la matrice de trafic a échoué. Document invalide à : {}"
#define ERR_MSG_42 ICON_CROSS " Matrice de trafic : La demande #{} a une taille de tableau de valeurs incohérente (attendu {}, obtenu {})"
#define ERR_MSG_43 ICON_CROSS " Matrice de trafic : Demande dupliquée trouvée (s={}, t={})"
#define ERR_MSG_44 ICON_CROSS " Le fichier de schéma de scénario '{}' est introuvable ou n'est pas un JSON valide"
#define ERR_MSG_45 ICON_CROSS " La validation JSON du scénario a échoué. Schéma invalide à : {}"
#define ERR_MSG_46 ICON_CROSS " La validation JSON du scénario a échoué. Mot-clé invalide : {}"
#define ERR_MSG_47 ICON_CROSS " La validation JSON du scénario a échoué. Document invalide à : {}"
#define ERR_MSG_48 ICON_CROSS " Scénario : Entrée de budget dupliquée pour le pas de temps {}"
#define ERR_MSG_49 ICON_CROSS " Scénario : Entrée d'intervention dupliquée pour le pas de temps {}"
#define ERR_MSG_50 ICON_CROSS " Scénario : Budget/Intervention au pas de temps 0 non autorisé (t doit être ≥ 1)"
#define ERR_MSG_51 ICON_CROSS " Le fichier de schéma des chemins SR '{}' est introuvable ou n'est pas un JSON valide"
#define ERR_MSG_52 ICON_CROSS " La validation JSON des chemins SR a échoué. Schéma invalide à : {}"
#define ERR_MSG_53 ICON_CROSS " La validation JSON des chemins SR a échoué. Mot-clé invalide : {}"
#define ERR_MSG_54 ICON_CROSS " La validation JSON des chemins SR a échoué. Document invalide à : {}"
#define ERR_MSG_55 ICON_CROSS " Chemins SR : Entrée de chemin SR dupliquée pour la demande {} au pas de temps {}"
#define ERR_MSG_56 ICON_CROSS " Seules {} sur {} demandes ont été routées au pas de temps {}. Cela peut indiquer un problème avec les chemins SR ou le scénario d'interventions."
#define ERR_MSG_57 ICON_CROSS " Le checker n'a pas été compilé avec le support de FTXUI. Veuillez mettre ENABLE_FTXUI à 1 et recompiler."

// Info messages
#define INFO_MSG_0 ICON_CHECK " Topologie réseau '{}' chargée avec succès ({} nœuds, {} arcs)"
#define INFO_MSG_1 ICON_CHECK " Graphe de demandes '{}' chargé avec succès ({} demandes)"
#define INFO_MSG_2 ICON_CHECK " Scénario d'entrée '{}' chargé avec succès (taille budget {}, interventions {})"
#define INFO_MSG_3 ICON_CHECK " Chemins SR d'entrée '{}' chargés avec succès ({} chemins SR)"

#define INFO_MSG_4 "Utilisation Maximale de Lien (MLU) à {} : {} (score de charge inverse : {})"
#define INFO_MSG_5 "Chargement des chemins SR depuis '{}'..."
#define INFO_MSG_6 "Chargement de la topologie réseau depuis '{}'..."
#define INFO_MSG_7 "Chargement de la matrice de trafic depuis '{}'..."
#define INFO_MSG_8 "Chargement du scénario depuis '{}'..."
#define INFO_MSG_9 "Validation du format JSON réseau avec le schéma '{}'..."
#define INFO_MSG_10 ICON_CHECK " Validation du format JSON réseau réussie"
#define INFO_MSG_11 "Validation du format JSON de la matrice de trafic avec le schéma '{}'..."
#define INFO_MSG_12 ICON_CHECK " Validation du format JSON de la matrice de trafic réussie"
#define INFO_MSG_13 "Validation du format JSON du scénario avec le schéma '{}'..."
#define INFO_MSG_14 ICON_CHECK " Validation du format JSON du scénario réussie"
#define INFO_MSG_15 "Validation du format JSON des chemins SR avec le schéma '{}'..."
#define INFO_MSG_16 ICON_CHECK " Validation du format JSON des chemins SR réussie"

#endif
