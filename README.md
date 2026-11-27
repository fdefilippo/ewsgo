# EWSGo

<img src="assets/ewsgo-logo.png" alt="EWSGo logo" width="112">

[Italiano](README.it.md)

EWSGo is an open-source gateway that exposes Exchange Web Services (EWS) and
MAPI over HTTP to clients of other mail servers. It is written in Go. Zimbra
is the first backend, accessed through its native SOAP and REST interfaces.
Protocol frontends share a backend-independent core. JMAP is planned as the
second backend.

Developed by **Defilippo srls**. <info@defilippo.org> · <https://www.defilippo.org>

The MAPI remote operations (ROPs) follow Microsoft's Exchange Server Protocol
specifications. [MS-OXCROPS](https://learn.microsoft.com/en-us/openspecs/exchange_server_protocols/ms-oxcrops/13af6911-27e5-4aa0-bb75-637b02d4f2ef)

## Compatibility

EWSGo is in preview. Lab tests on Zimbra FOSS 10.1.20 covered mail
synchronization with MailCraft CLI and mail access in classic Outlook LTSC
2021 in online and cached modes. Testing also covered personal contacts and
appointments in Outlook, and recurring events and meeting invitations over
EWS. Lab tests also verified recurring-event edits and meeting invitation
responses in classic Outlook, and synchronization of a mixed Zimbra Trash
folder containing 340 messages. Initial cached synchronization can take
longer than reopening a populated Outlook profile.

The Zimbra backend includes `ewsgo-rules` for Outlook rules, receive folders
and deferred actions. Its source and installation guide are in
`extensions/zimbra-rules`. Install it on the Zimbra mailbox server to enable
those features.

## Install and start from a package

Install the x86-64 RPM for Enterprise Linux 8, 9 or 10, or the DEB for Debian
13 or Ubuntu 22.04, 24.04 or 26.04 LTS. The service starts disabled.

Edit `/etc/ewsgo/ewsgo.yaml` and set `listen`, `zimbra-origin`, `tls-cert`
and `tls-key`. The `ewsgo` service account needs read access to the TLS key
and stores its database at `/var/lib/ewsgo/ewsgo.db`.

```sh
sudo systemctl enable --now ewsgo.service
```

Point the client to `https://gateway.example.org:8443/EWS/Exchange.asmx` and
sign in with the Zimbra account. See the [Zimbra setup guide](docs/zimbra.md)
for TLS, Outlook and MailCraft.

## Build from source

Use Go 1.27.4 or later. Start a local process to check liveness.

```sh
make build
./bin/ewsgo -listen 127.0.0.1:8080
# In another terminal
curl --fail http://127.0.0.1:8080/health/live
```

For standalone EWS operation, pass the YAML file with `-config` or supply
the gateway options directly as flags.

Run `make check` to verify formatting, run static checks and tests, and build
the binaries. The checks require Go, a C compiler for the race detector, and
Python 3. The Linux packages workflow builds, installs and checks each
package in its target distribution and uploads it with a SHA-256 checksum.

## License

[GNU Affero General Public License v3.0 or later](LICENSE).
