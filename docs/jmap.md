# JMAP Mail backend preview

EWSGo connects a personal JMAP Mail account to EWS. The gateway uses JMAP Core
and Mail for discovery, folder and message reads, blob downloads and native
synchronization. It can also submit MIME and retain a copy in Sent when the
account advertises JMAP Submission.
See [RFC 8620](https://www.rfc-editor.org/rfc/rfc8620) and
[RFC 8621](https://www.rfc-editor.org/rfc/rfc8621).

## Configuration

Select JMAP in the YAML configuration with a Session URL. The Session endpoint,
the API and the blob download endpoint must have the same HTTPS origin. The
gateway uses the operating system trust store for the backend certificate.
The server must accept HTTP Basic authentication over TLS and advertise a
personal primary account for JMAP Mail. Absolute and relative Session URLs
for API, download and upload are supported within the same origin. The adapter
also accepts address-shaped account and identity IDs, as observed on Cyrus.

```yaml
listen: "127.0.0.1:8443"
jmap-session-url: https://mail.example.org/.well-known/jmap
tls-cert: /etc/ewsgo/fullchain.pem
tls-key: /etc/ewsgo/key.pem
store-path: /var/lib/ewsgo/ewsgo.db
```

Configure one backend per gateway process. In MailCraft use the EWS URL
`https://<gateway>/EWS/Exchange.asmx` and the JMAP account credentials.
Keep the same SQLite database and Session URL across restarts to preserve EWS
IDs and sync cursors. The gateway authenticates each EWS request with the
JMAP server.

## Synchronization

One JMAP Email may belong to multiple Mailboxes. EWSGo assigns a distinct EWS
item ID to each folder membership. Removing one membership invalidates only
that folder's ID. Each synchronization follows the native `Mailbox/changes` and `Email/changes`
cursors. EWSGo reads the affected emails and verifies the final server states
before committing the cursor and the journal together. A complete paginated
snapshot establishes the initial state and recovers an expired cursor.

The preview covers the folder tree, normal email synchronization and raw MIME
up to 1 MiB per message. Native attachment descriptors and decoded bytes are
available to the backend contract. MailCraft downloads attachments from MIME
fetched through EWS. The adapter scans up to 1,000 folders, 10,000 emails and
20,000 folder memberships per account. Newly visible folders with existing
messages can trigger a full scan.

## Submission and limits

EWS `CreateItem SendAndSaveCopy` accepts full MIME containing a valid
`Message-ID` and a From address matching a declared JMAP identity. The JMAP
account must be writable and capable of submission, with writable Drafts and
Sent roles. EWSGo uploads the MIME, imports a temporary draft, then requests
submission and a Sent copy. A successful response confirms both JMAP
acknowledgments. An ambiguous response prevents further dispatch using that
Message-ID. Repeating an acknowledged request with the same MIME returns its
recorded result. Reusing the Message-ID with changed MIME returns a conflict.

Folder and message edits, managed drafts, sharing, groupware and MAPI are
outside this JMAP profile. The gateway uses the EWS response for an unavailable
operation. It serves no MAPI endpoints with JMAP configured.

## Qualification

Cyrus IMAP 3.10.2 on Debian 13 served as an independent, local JMAP endpoint
on 27 November 2026. The run used JMAP Core, Mail and Submission. MailCraft CLI
0.15.0 and the MailCraft EWS host synchronized the same message in Inbox and a
second folder. After removing the second membership, restarting the gateway
and reusing both stores, MailCraft removed only that placement and retained
the Inbox ID. A separate GetItem and MailCraft saveAttachment recovered all
256 bytes of the binary fixture. The shared Mail profile suite also verified
the original stored MIME byte for byte and the native attachment reader.

EWS submission to Cyrus produced one message at an isolated SMTP receiver
and one native item in Sent. Resending identical MIME returned success with
no second dispatch. Changing the MIME while retaining the Message-ID
returned a conflict. The real endpoint also returned
`ErrorInvalidPropertyForOperation` for an unsupported folder write and HTTP
404 for MAPI. These are observations on the tested server and account.
The supported contract is expressed in standard JMAP capabilities.
[Cyrus JMAP setup](https://www.cyrusimap.org/3.12/imap/download/installation/http/jmap.html)

## Reproduce the checks

```sh
go test ./internal/service -run TestJMAPPersistsMembershipAcrossRestart -v
```

The test uses an independent JMAP fixture over TLS, exercises pagination,
rejects an incomplete snapshot, drops a native delta response mid-pagination
and reuses EWS sync cursors after reopening SQLite and recreating the gateway
handler. It verifies that a routine delta avoids enumerating emails. The MIME for the surviving EWS ID
is compared byte for byte. Set `EWSGO_TEST_MAILCRAFT_RUNNER` to the absolute path
of the built [MailCraft EWS host](../harness/mailcraft/build.py) to also run the
test with `/usr/bin/mailcraft-cli` and verify the contents of its local store.
For the shared suite and instructions to add another backend, see
[Backend contract v1](backend-contract-v1.md). To reproduce the live MailCraft
cycle, run the MailCraft harness with `--profile jmap-network`, the EWS URL, the synthetic
account credentials, the gateway CA and a persistent `--store`. Create a
message with two memberships through JMAP Mail. Remove just one membership
with an `Email/set` patch, restart the same gateway with the same SQLite
file, and rerun the harness on the same store. The results should show
the removed placement and leave the surviving message ID intact.
