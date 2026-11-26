# Estensione filtri in ingresso per Zimbra

L'estensione aggiunge operazioni SOAP nella namespace
`urn:ewsgo:zimbra:rules:1`, sul normale endpoint di mailbox. Accetta soltanto
il token dell'utente proprietario di una mailbox locale con accesso alle
preferenze. Zimbra verifica l'autenticazione attraverso il suo dispatcher.

- `ReadIncomingRulesRequest` restituisce una revisione e lo script Sieve
  integrale in Base64. Include una proiezione nativa `GetFilterRulesResponse`
  quando la riconversione riproduce esattamente lo script.
- `ReplaceIncomingRulesRequest` richiede l'attributo `expected` con la revisione
  acquisita in lettura e un `ModifyFilterRulesRequest` Zimbra. Il risultato è
  `applied`, `conflict` se l'entry LDAP è cambiata, oppure `unsupported` se
  lo script corrente non ha una proiezione integrale. Dopo un errore di
  trasporto il client deve rileggere prima di riprovare.

- `ReadReceiveFoldersRequest` legge le associazioni MAPI conservate nelle
  impostazioni native della mailbox.
- `SetReceiveFolderRequest` crea o sostituisce un’associazione per classe.
  Il valore `folder="0"` rimuove un’associazione specifica. Le classi
  `IPM` e `Report.IPM` hanno le destinazioni predefinite del protocollo.

Il callback LMTP muove i messaggi ancora in Inbox dopo l’azione dei filtri
nativi. La classificazione usa `IPM.Note` per la posta ordinaria e
`Report.IPM.Note.*` per le notifiche MIME di consegna. Le associazioni
a classi che LMTP non materializza restano consultabili. Un errore nel
callback lascia la consegna già confermata in Inbox. I movimenti sono
nativi e vengono osservati dal normale journal di Zimbra.

- `ReadDeferredActionsRequest` legge i DAM della mailbox autenticata. La
  lettura della DAF chiede la riconciliazione con `reconcile="1"`.
  La variante `targets="1"` restituisce soltanto gli ID dei messaggi
  con almeno un DAM registrato e serve il flag MAPI del messaggio originale.
- `PatchDeferredActionsRequest` aggiorna insieme i riferimenti dei DAM
  corrispondenti al riferimento server. La mailbox conserva anche
  l'identità originale del messaggio.
- `DeleteDeferredActionRequest` rimuove la singola azione elaborata.

Per un'azione differita, il filtro Sieve applica un tag tecnico con
l'impronta della definizione, senza copiare l'azione nel MIME. La
definizione viene salvata prima del CAS LDAP, così anche una consegna
avviata sotto la revisione precedente può risolverla. Il callback LMTP
rimuove il tag dal messaggio dopo aver registrato il DAM. Zimbra
può mostrare il nome del tag tecnico nel proprio elenco di tag.
La coda attiva usa lo stesso limite dell'indice di contenuti EWSGo
(1.024 righe). Se è piena, l'estensione conserva il tag sul nuovo
messaggio e scrive un avviso. La riconciliazione ricerca i tag EWSGo
con una definizione registrata, anche nel Cestino e in Spam, fino a 64
messaggi per lettura. Salva le azioni prima di staccare i tag. Quando il
client consuma un DAM, l'estensione stacca l'eventuale marker residuo prima
di cancellare il record. Il recupero usa la cartella osservata al momento
in cui il record viene materializzato.

Il flag del messaggio originale è vero finché resta almeno un DAM registrato
per quell'ID. La proiezione è disponibile in GetPropertiesSpecific, nelle
tabelle dei messaggi e nei delta ICS normali. Il cambio del flag aggiorna il
checkpoint anche a revisione MIME invariata. I tag storici e le definizioni
sono conservati per le consegne che stanno ancora usando una revisione
precedente dei filtri.

Una regola standard può partire con `OP_MARK_AS_READ` seguito dalle
azioni differite. Zimbra applica il flag di lettura in Sieve e il DAM
contiene soltanto le azioni che deve ancora eseguire Outlook.
Le regole conservano anche il nome del provider e i suoi dati opachi.
Il DAM conserva la variante del provider attiva alla consegna.
Outlook può scrivere e rileggere `PidTagRwRulesStream` sulle sue
FAI `IPM.RuleOrganizer` e `IPM.RuleOrganizer.ClientRules`. EWSGo
conserva questi byte nel replica store della mailbox.

Nella DAF, Outlook legge anche il FID della cartella della regola
(`0x67420014`) e la data di creazione del DAM. La data resta stabile
dopo il backpatch. Per confermare l'elaborazione, Outlook può inviare
`RopDeleteMessages` con `NotifyNonRead`. Il gateway elimina il DAM
dalla coda interna. Il messaggio originale resta nella sua cartella.

### Raccolta dei marker obsoleti

Un marker può essere ancora in uso dopo la rimozione della regola.
La valutazione LMTP inizia prima del callback che registra il DAM.
Il lock della mailbox protegge la coda, mentre LMTP usa un lock
diverso per la consegna. Lo svuotamento della coda e una ricerca
senza risultati lasciano aperta la possibilità che una consegna
usi ancora la vecchia revisione.

La manutenzione richiede una barriera che impedisca le nuove consegne
e modifiche ai filtri sulla mailbox interessata e attenda il termine
delle consegne già accettate e dei relativi callback. Dopo la
barriera occorre inventariare la revisione Sieve effettiva, le
definizioni registrate, le azioni in coda e tutti gli oggetti con
marker ancora recuperabili. La scansione ordinaria della DAF
copre fino a 64 messaggi per lettura. Una definizione rimane necessaria se la
revisione attiva la usa, se un marker è ancora attaccato a un oggetto
o se la coda contiene un'azione corrispondente. La cancellazione
deve lasciare intatto lo script e può procedere soltanto con un
inventario completo e mentre la barriera resta attiva.

L'estensione conserva i tag tecnici e le definizioni storiche. La raccolta delle definizioni storiche richiede ancora la barriera di consegna.
La riconciliazione dei marker sui messaggi e la rimozione dei DAM elaborati
continuano durante l'uso ordinario.

Le sottoscrizioni MAPI sul contenuto della DAF usano le notifiche di
tabella. Una sottoscrizione esplicita sulla DAF può ricevere anche gli
eventi di creazione, modifica e cancellazione delle azioni osservate.

La revisione usa `entryCSN` e lo script dalla stessa lettura sul master LDAP.
L'asserzione LDAP controlla insieme revisione e identità prima di aggiornare
l'attributo nativo. L'estensione usa il parser Sieve e la cache dei filtri di
Zimbra. Il pacchetto è opzionale e va compilato per le librerie della build
Zimbra di destinazione.

## Installazione su un mailbox server

```sh
./build.sh /tmp/ewsgo-rules.jar
install -d -m0755 -o zimbra -g zimbra /opt/zimbra/lib/ext/ewsgo-rules
install -m0644 -o zimbra -g zimbra /tmp/ewsgo-rules.jar /opt/zimbra/lib/ext/ewsgo-rules/
runuser -u zimbra -- /opt/zimbra/bin/zmmailboxdctl restart
```

La build è stata verificata su Zimbra FOSS 10.1.20.p1,
con JDK 17. L'installazione richiede che il mailbox server carichi l'estensione
e che i client usino la normale connessione SOAP TLS.
