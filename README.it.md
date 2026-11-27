# EWSGo

<img src="assets/ewsgo-logo.png" alt="Logo EWSGo" width="112">

[English](README.md)

EWSGo è un gateway open source che espone Exchange Web Services (EWS) e MAPI
su HTTP ai client di altri server di posta. È scritto in Go. Il primo backend
è Zimbra, tramite le sue interfacce native SOAP e REST. I frontend di
protocollo condividono un core indipendente dal backend.

Realizzato da **Defilippo srls**. <info@defilippo.org> · <https://www.defilippo.org>

Le operazioni remote MAPI (ROP) seguono le specifiche Microsoft dei protocolli
Exchange Server. [MS-OXCROPS](https://learn.microsoft.com/en-us/openspecs/exchange_server_protocols/ms-oxcrops/13af6911-27e5-4aa0-bb75-637b02d4f2ef)

## Compatibilità

EWSGo è in anteprima. Le prove su Zimbra FOSS 10.1.20 hanno verificato la
sincronizzazione della posta con MailCraft CLI e l’accesso da Outlook classico
LTSC 2021 in modalità online e cache. Le prove hanno coperto anche contatti e
appuntamenti personali in Outlook, ed eventi ricorrenti e inviti via EWS.
Le prove hanno verificato anche la modifica delle ricorrenze e le risposte
agli inviti da Outlook classico, oltre alla sincronizzazione di un Cestino
Zimbra misto con 340 messaggi. La prima sincronizzazione in cache può
richiedere più tempo della riapertura di un profilo Outlook già popolato.

Il [backend JMAP Mail in anteprima](docs/jmap.md) offre sincronizzazione EWS,
lettura di MIME e allegati e invio MIME quando il server dichiara JMAP
Submission. MailCraft è stato verificato su un server Cyrus indipendente.
[Contratto backend v1](docs/backend-contract-v1.md)

Il backend Zimbra include `ewsgo-rules` per le regole di Outlook, le cartelle
di ricezione e le azioni differite. I sorgenti e la guida di installazione
sono in `extensions/zimbra-rules`. Installa l’estensione sul server mailbox
di Zimbra per abilitare queste funzioni.

## Installazione e avvio da pacchetto

Installa il pacchetto x86-64 RPM per Enterprise Linux 8, 9 o 10 o il DEB
per Debian 13 o Ubuntu 22.04, 24.04 o 26.04 LTS. Il servizio è inizialmente
disabilitato.

Imposta `listen`, `tls-cert`, `tls-key` e un endpoint di backend
(`zimbra-origin` oppure `jmap-session-url`) in `/etc/ewsgo/ewsgo.yaml`.
L’utente di servizio `ewsgo` deve poter leggere la chiave TLS e salva il
database in `/var/lib/ewsgo/ewsgo.db`.

```sh
sudo systemctl enable --now ewsgo.service
```

Configura il client EWS su `https://gateway.example.org:8443/EWS/Exchange.asmx`
e accedi con l’account del backend. [Guida Zimbra](docs/zimbra.md) ·
[Guida JMAP](docs/jmap.md)

## Compilazione dai sorgenti

Usa Go 1.27.4 o successivo. Avvia un processo locale per verificarne lo stato.

```sh
make build
./bin/ewsgo -listen 127.0.0.1:8080
# In un altro terminale
curl --fail http://127.0.0.1:8080/health/live
```

Per avviare EWS direttamente, passa il file YAML con `-config` oppure
imposta le opzioni del gateway da riga di comando.

Esegui `make check` per verificare la formattazione, eseguire i controlli
statici e i test e compilare i binari. Servono Go, un compilatore C per il
race detector e Python 3. Il workflow Linux packages costruisce, installa e
verifica ciascun pacchetto nella sua distribuzione, poi lo carica con la
relativa impronta SHA-256.

## Licenza

[GNU Affero General Public License v3.0 o successiva](LICENSE).
