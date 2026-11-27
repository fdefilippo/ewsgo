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
EWS. Outlook writes to recurring events and Outlook invitation actions still
need qualification. A later Outlook cached-mode test verified synchronization
of 340 messages from a Zimbra Trash folder containing mail, contacts and
appointments.

The Zimbra backend includes `ewsgo-rules` for Outlook rules, receive folders
and deferred actions. Its source and installation guide are in
`extensions/zimbra-rules`. Install it on the Zimbra mailbox server to enable
those features.

## Quick start

Install the RPM for Enterprise Linux 8, 9 or 10, or the DEB for Debian 13 or
Ubuntu 22.04, 24.04 or 26.04 LTS when available. The initial packages target
x86-64 and install the `ewsgo` executable. To build from source, use Go
1.27.4 or later and run `make build`. Use `./bin/ewsgo` in the commands below
if running from the build directory.

```sh
ewsgo -listen 127.0.0.1:8080
# In another terminal
curl --fail http://127.0.0.1:8080/health/live
```

To enable EWS, use a trusted HTTPS Zimbra origin, a certificate for the
gateway, and a writable directory for its SQLite state.

```sh
ewsgo -listen 0.0.0.0:8443 \
  -tls-cert /path/to/fullchain.pem -tls-key /path/to/key.pem \
  -zimbra-origin https://mail.example.org -store-path ./ewsgo.db
```

Point the client to `https://gateway.example.org:8443/EWS/Exchange.asmx` and
sign in with the Zimbra account. Outlook uses a separate MAPI/HTTP profile.
See the [Zimbra setup guide](docs/zimbra.md) for Outlook, MailCraft and the
gateway options.

Run `make check` to verify formatting, run static checks and tests, and build
the binaries. The checks require Go, a C compiler for the race detector, and
Python 3. The Linux packages workflow builds, installs and checks each
package in its target distribution and uploads it with a SHA-256 checksum.

## License

[GNU Affero General Public License v3.0 or later](LICENSE).
