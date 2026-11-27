# Zimbra quick start

EWSGo uses Zimbra's native SOAP and REST interfaces. The alpha was exercised
with a Zimbra FOSS 10.1.20 build from [Maldua](https://github.com/maldua/zimbra-foss).
Start with a dedicated mailbox. The gateway uses the Zimbra account
credentials presented by the client.

## Gateway

The RPM and DEB packages install `/etc/ewsgo/ewsgo.yaml` and `ewsgo.service`.
Edit the YAML to set the listen address, Zimbra HTTPS origin and TLS file
paths. Import a private Zimbra issuer CA into the gateway host's trust store
if needed. Keep the TLS private key readable by the `ewsgo` service user
(for example `root:ewsgo` with mode `0640`). Use a certificate trusted by
the clients. Store mailbox credentials in the client as usual.

The packaged service runs as `ewsgo` and stores SQLite state at
`/var/lib/ewsgo/ewsgo.db`. That directory is private to the service user.
The service can write there. An alternate state directory outside
`/var/lib/ewsgo` needs appropriate ownership and a systemd `ReadWritePaths`
override. Use absolute paths in the YAML. Upgrades preserve a locally
modified configuration and the SQLite state.

Enable and start the service after editing the template.

```sh
sudo systemctl enable --now ewsgo.service
```

To run the same configuration directly, use `ewsgo -config /etc/ewsgo/ewsgo.yaml`.
Explicit CLI flags override YAML entries. The original flags work without a
configuration file, for example when running a standalone binary.

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

```yaml
listen: "0.0.0.0:443"
tls-cert: /etc/ewsgo/fullchain.pem
tls-key: /etc/ewsgo/key.pem
zimbra-origin: https://mail.example.org
store-path: /var/lib/ewsgo/ewsgo.db
public-origin: https://gateway.example.org
deployment-id: REPLACE_WITH_A_STABLE_UUID
outlook-lab: true
outlook-profile-spike: true
mapi-spike: true
mapi-session-spike: true
mapi-store-spike: true
mapi-persistent-replica: true
nspi-bind-spike: true
nspi-directory-spike: true
mapi-hierarchy-download-spike: true
mapi-contents-download-spike: true
mapi-contents-table-spike: true
mapi-fai-collector-spike: true
mapi-drafts-reference-spike: true
mapi-ost-property-spike: true
mapi-hierarchy-import: true
mapi-read-state-import: true
mapi-folder-aggregates: true
mapi-contacts-metadata-spike: true
mapi-calendar-metadata-spike: true
```

The packaged service can bind port 443 as `ewsgo`. Restart the service after
editing the YAML. Create a new
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
