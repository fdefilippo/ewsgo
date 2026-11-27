# Zimbra quick start

EWSGo uses Zimbra's native SOAP and REST interfaces. The alpha was exercised
with a Zimbra FOSS 10.1.20 build from [Maldua](https://github.com/maldua/zimbra-foss).
Start with a dedicated mailbox. The gateway uses the Zimbra account
credentials presented by the client.

## Gateway

Use an HTTPS origin for Zimbra. Import the Zimbra issuer CA into the gateway
host's trust store when using a private CA. Give the gateway a certificate
trusted by the clients and a private directory writable by its service user
for the SQLite database.

```sh
ewsgo -listen 0.0.0.0:8443 \
  -tls-cert /etc/ewsgo/fullchain.pem -tls-key /etc/ewsgo/key.pem \
  -zimbra-origin https://mail.example.org \
  -store-path /var/lib/ewsgo/ewsgo.db
```

EWS is available at `https://gateway.example.org:8443/EWS/Exchange.asmx`.
Use Basic authentication over TLS. `/health/live` reports process liveness.
The alpha returns 503 from `/health/ready`. The gateway logs authenticated
account addresses. Choose a JSON log threshold with `-loglevel`.

## Classic Outlook

For a dedicated test domain, point `autodiscover.example.org` and
`gateway.example.org` at the gateway and use HTTPS on port 443. Use a
certificate valid for both names and a permanent deployment UUID. Keep the
UUID and the SQLite database when restarting the gateway.

```sh
ewsgo -listen 0.0.0.0:443 \
  -tls-cert /etc/ewsgo/fullchain.pem -tls-key /etc/ewsgo/key.pem \
  -zimbra-origin https://mail.example.org \
  -store-path /var/lib/ewsgo/ewsgo.db \
  -public-origin https://gateway.example.org \
  -deployment-id REPLACE_WITH_A_STABLE_UUID \
  -outlook-lab -outlook-profile-spike -mapi-spike \
  -mapi-session-spike -mapi-store-spike -mapi-persistent-replica \
  -nspi-bind-spike -nspi-directory-spike \
  -mapi-hierarchy-download-spike -mapi-contents-download-spike \
  -mapi-contents-table-spike -mapi-fai-collector-spike \
  -mapi-drafts-reference-spike -mapi-ost-property-spike \
  -mapi-hierarchy-import -mapi-read-state-import -mapi-folder-aggregates \
  -mapi-contacts-metadata-spike -mapi-calendar-metadata-spike
```

Run under a dedicated OS user with permission to bind port 443. Create a new
classic Outlook profile, add the test address as Microsoft Exchange, and
verify Inbox before testing writes. The optional `ewsgo-rules` extension is
installed on the Zimbra mailbox server. Its source and build instructions
are in [extensions/zimbra-rules](https://github.com/fdefilippo/ewsgo/tree/main/extensions/zimbra-rules).

## MailCraft CLI

The source checkout includes a headless harness that builds the MailCraft
EWS runner from a pinned revision and passes the results to MailCraft CLI.
With the CLI installed and Cargo and Python available, build the runner.

```sh
python3 harness/mailcraft/build.py --cache /tmp/ewsgo-mailcraft
```

Create a private, owner-only file outside the repository with `user:` and
`pass:` entries for the test mailbox. Use the runner path printed by the
build command and the EWS URL of your gateway.

```sh
python3 harness/mailcraft/run.py \
  --runner /tmp/ewsgo-mailcraft/host/target/debug/ewsgo-mailcraft-host \
  --cli /usr/bin/mailcraft-cli --mode sync \
  --credentials /path/to/private-credentials \
  --url https://gateway.example.org:8443/EWS/Exchange.asmx
```

Pass `--ca /path/to/issuer.pem` if the client uses a private issuer.

## Alpha limits

Lab tests covered recurring series and individual occurrences, and sending,
cancelling, accepting, tentatively accepting and declining invitations in
classic Outlook. Cached Outlook initially populated Inbox and a mixed Trash
folder with 340 messages. Subsequent mail in an additional test folder
synchronized both automatically and on manual refresh. Timing varied by
folder and by the client’s synchronization phase.
One older invitation stayed absent from a reused Outlook cache after a
manual refresh. The gateway exposed it over MAPI and a fresh Outlook cache
displayed it.

Deployment qualification for load, recovery and multiple gateway instances
is still pending. Share client and server versions, client mode and a short
description of the scenario when reporting a result.
