# EWSGo

<img src="assets/ewsgo-logo.png" alt="Logo EWSGo" width="112">

[English](README.md)

EWSGo è un gateway open source che espone Exchange Web Services (EWS) e MAPI
su HTTP ai client di altri server di posta. È scritto in Go. Il primo backend
è Zimbra, tramite le sue interfacce native SOAP e REST. I frontend di
protocollo condividono un core indipendente dal backend. JMAP è previsto
come secondo backend.

Realizzato da **Defilippo srls**. <info@defilippo.org> · <https://www.defilippo.org>

Le operazioni remote MAPI (ROP) seguono le specifiche Microsoft dei protocolli
Exchange Server. [MS-OXCROPS](https://learn.microsoft.com/en-us/openspecs/exchange_server_protocols/ms-oxcrops/13af6911-27e5-4aa0-bb75-637b02d4f2ef)

## Compatibilità

EWSGo è in anteprima. Le prove su Zimbra FOSS 10.1.20 hanno verificato la
sincronizzazione della posta con MailCraft CLI e l’accesso da Outlook classico
LTSC 2021 in modalità online e cache. Le prove hanno coperto anche contatti e
appuntamenti personali in Outlook, ed eventi ricorrenti e inviti via EWS.
Restano da qualificare le scritture sulle ricorrenze e le operazioni sugli
inviti da Outlook. Un problema noto riguarda la sincronizzazione di Cestini
Zimbra grandi che contengono insieme posta ed elementi groupware.

Il backend Zimbra include `ewsgo-rules` per le regole di Outlook, le cartelle
di ricezione e le azioni differite. I sorgenti e la guida di installazione
sono in `extensions/zimbra-rules`. Installa l’estensione sul server mailbox
di Zimbra per abilitare queste funzioni.

## Avvio rapido

Quando disponibili, installa il pacchetto RPM per Enterprise Linux 8, 9 o 10
oppure il DEB per Debian 13 o Ubuntu 22.04, 24.04 o 26.04 LTS. I primi
pacchetti sono per x86-64 e installano l’eseguibile `ewsgo`. Per compilare dai
sorgenti, usa Go 1.27.4 o successivo ed esegui `make build`. Usa
`./bin/ewsgo` nei comandi seguenti se lavori dalla directory di build.

```sh
ewsgo -listen 127.0.0.1:8080
# In un altro terminale
curl --fail http://127.0.0.1:8080/health/live
```

Per abilitare EWS, usa un’origine HTTPS Zimbra attendibile, un certificato
per il gateway e una directory scrivibile per lo stato SQLite.

```sh
ewsgo -listen 0.0.0.0:8443 \
  -tls-cert /percorso/fullchain.pem -tls-key /percorso/key.pem \
  -zimbra-origin https://mail.example.org -store-path ./ewsgo.db
```

Configura il client su `https://gateway.example.org:8443/EWS/Exchange.asmx`
e accedi con l’account Zimbra. Outlook usa un profilo MAPI/HTTP separato.
La [guida Zimbra](docs/zimbra.md) descrive la configurazione di Outlook,
MailCraft e del gateway.

Esegui `make check` per verificare la formattazione, eseguire i controlli
statici e i test e compilare i binari. Servono Go, un compilatore C per il
race detector e Python 3. Il workflow Linux packages costruisce, installa e
verifica ciascun pacchetto nella sua distribuzione, poi lo carica con la
relativa impronta SHA-256.

## Licenza

[GNU Affero General Public License v3.0 o successiva](LICENSE).
