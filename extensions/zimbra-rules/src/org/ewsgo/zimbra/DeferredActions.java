// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package org.ewsgo.zimbra;

import java.io.IOException;
import java.nio.ByteBuffer;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.Base64;
import java.util.HexFormat;
import java.util.HashSet;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.TreeMap;
import java.util.regex.Pattern;

import org.dom4j.QName;

import com.zimbra.common.service.ServiceException;
import com.zimbra.common.soap.Element;
import com.zimbra.common.util.ZimbraLog;
import com.zimbra.cs.account.Account;
import com.zimbra.cs.index.SearchParams;
import com.zimbra.cs.index.SortBy;
import com.zimbra.cs.index.ZimbraQueryResults;
import com.zimbra.cs.lmtpserver.LmtpCallback;
import com.zimbra.cs.mailbox.MailItem;
import com.zimbra.cs.mailbox.Mailbox;
import com.zimbra.cs.mailbox.MailboxManager;
import com.zimbra.cs.mailbox.MailServiceException;
import com.zimbra.cs.mailbox.Message;
import com.zimbra.cs.mailbox.Metadata;
import com.zimbra.cs.mailbox.OperationContext;
import com.zimbra.cs.mailbox.Tag;
import com.zimbra.cs.mime.ParsedMessage;
import com.zimbra.soap.DocumentDispatcher;
import com.zimbra.soap.DocumentHandler;
import com.zimbra.soap.ZimbraSoapContext;
import com.zimbra.soap.mail.type.FilterAction;
import com.zimbra.soap.mail.type.FilterRule;

final class DeferredActions implements LmtpCallback {
    private static final String NS = IncomingRulesExtension.NS;
    private static final String SECTION = "ewsgoDeferredActions1";
    private static final String DEFINITION = "ewsgoDamDefinition1-";
    private static final String SUFFIX = " [ewsgo-dam-v1:";
    private static final String MARKER = "ewsgo-dam-v1-";
    private static final Pattern MARKER_NAME = Pattern.compile(MARKER + "[a-f0-9]{32}");
    private static final int MAX_WIRE = 510;
    private static final int MAX_PROVIDER_BYTES = 64;
    private static final int MAX_PROVIDER_DATA_BYTES = 255;
    private static final int MAX_DEFINITION = 8 + 2 + MAX_PROVIDER_BYTES + MAX_PROVIDER_DATA_BYTES + MAX_WIRE;
    private static final int MAX_RECORDS = 1024; // Gateway mail contents index limit.
    private static final int RECOVERY_BATCH = 64;
    private static final List<String> OPERATIONS = List.of("ReadDeferredActions", "PatchDeferredActions", "DeleteDeferredAction");
    private volatile boolean active;

    void setActive(boolean enabled) {
        active = enabled;
    }

    void register(DocumentDispatcher dispatcher) {
        for (String operation : OPERATIONS) {
            dispatcher.registerHandler(QName.get(operation + "Request", NS), new Handler(operation));
        }
    }

    void deregister(DocumentDispatcher dispatcher) {
        for (String operation : OPERATIONS) {
            dispatcher.unRegisterHandler(QName.get(operation + "Request", NS));
        }
    }

    private record Dam(String id, int folder, int message, String definition, String client, long created, long time) {
        Dam patch(String entry, long now) {
            return new Dam(id, folder, message, definition, entry, created, Math.max(time + 1, now));
        }
    }

    private record AttachedMarker(int message, String tag) {}

    // Record the definition before activating the Sieve script. A delivery can
    // finish using the previous script after a CAS replaces it. Its marker must
    // still resolve to that version of the opaque action, never to a newer one.
    static void rememberRules(Account account, List<FilterRule> rules) throws ServiceException {
        Map<String, String> definitions = new TreeMap<>();
        for (FilterRule rule : rules) {
            int separator = rule.getName().indexOf(SUFFIX);
            if (separator < 0) {
                continue;
            }
            String definition = rule.getName().substring(separator + SUFFIX.length());
            byte[] wire = decode(definition, MAX_DEFINITION, true);
            String digest = digest(wire);
            if (!validDefinition(wire) || rule.getFilterActions().stream().noneMatch(action ->
                    action instanceof FilterAction.TagAction tag && (MARKER + digest).equals(tag.getTag()))) {
                throw ServiceException.INVALID_REQUEST("invalid deferred action rule", null);
            }
            definitions.put(digest, definition);
        }
        if (definitions.isEmpty()) {
            return;
        }
        Mailbox mailbox = MailboxManager.getInstance().getMailboxByAccount(account);
        OperationContext op = new OperationContext(account);
        mailbox.lock(true);
        try {
            for (var entry : definitions.entrySet()) {
                String key = DEFINITION + entry.getKey();
                Metadata existing = mailbox.getConfig(op, key);
                if (existing != null) {
                    if (!entry.getValue().equals(existing.get("wire"))) {
                        throw ServiceException.FAILURE("deferred action definition conflict", null);
                    }
                    continue;
                }
                Metadata data = new Metadata();
                data.put("wire", entry.getValue());
                mailbox.setConfig(op, key, data);
            }
        } finally {
            mailbox.unlock();
        }
    }

    private static String digest(byte[] wire) {
        try {
            byte[] hash = MessageDigest.getInstance("SHA-256").digest(wire);
            return HexFormat.of().formatHex(hash, 0, 16);
        } catch (NoSuchAlgorithmException e) {
            throw new IllegalStateException("SHA-256 unavailable", e);
        }
    }

    private static byte[] decode(String value, int limit, boolean url) throws ServiceException {
        if (value == null || value.length() > 4 * ((limit + 2) / 3)) {
            throw ServiceException.INVALID_REQUEST("invalid deferred action length", null);
        }
        try {
            byte[] wire = (url ? Base64.getUrlDecoder() : Base64.getDecoder()).decode(value);
            if (wire.length > limit) {
                throw ServiceException.INVALID_REQUEST("invalid deferred action length", null);
            }
            return wire;
        } catch (IllegalArgumentException e) {
            throw ServiceException.INVALID_REQUEST("invalid deferred action encoding", null);
        }
    }

    private static boolean validDefinition(byte[] wire) {
        if (wire.length < 10) {
            return false;
        }
        long id = ByteBuffer.wrap(wire).getLong();
        if ((id & Long.MAX_VALUE) == 0) {
            return false;
        }
        int start = 8;
        if (id < 0) {
            int provider = Byte.toUnsignedInt(wire[8]);
            int data = Byte.toUnsignedInt(wire[9]);
            start += 2 + provider + data;
            if (provider > MAX_PROVIDER_BYTES || start + 2 > wire.length) {
                return false;
            }
            if (provider != 0) {
                String name = new String(wire, 10, provider, StandardCharsets.US_ASCII);
                if (!name.startsWith("RuleOrganizer") || !name.equals(name.strip()) ||
                        name.chars().anyMatch(c -> c < 0x20 || c > 0x7e)) {
                    return false;
                }
            }
        }
        if (wire.length - start > MAX_WIRE) {
            return false;
        }
        int count = uint16(wire, start);
        int pos = start + 2;
        for (int i = 0; i < count; i++) {
            if (wire.length - pos < 11) {
                return false;
            }
            int size = uint16(wire, pos);
            if (size < 9 || size + 2 > wire.length - pos || wire[pos + 2] != 5) {
                return false;
            }
            pos += size + 2;
        }
        return count > 0 && pos == wire.length;
    }

    private static int uint16(byte[] wire, int pos) {
        return Byte.toUnsignedInt(wire[pos]) | Byte.toUnsignedInt(wire[pos + 1]) << 8;
    }

    private static Map<String, Dam> read(Mailbox mailbox, OperationContext op) throws ServiceException {
        Map<String, Dam> result = new TreeMap<>();
        Metadata data = mailbox.getConfig(op, SECTION);
        if (data == null) {
            return result;
        }
        long count = data.getLong("count", -1L);
        if (count < 0 || count > MAX_RECORDS) {
            throw ServiceException.FAILURE("invalid deferred action metadata", null);
        }
        for (int i = 0; i < count; i++) {
            String id = data.get("id" + i);
            long folder = data.getLong("f" + i, 0L);
            long message = data.getLong("m" + i, 0L);
            long time = data.getLong("t" + i, 0L);
            long created = data.getLong("created" + i, time);
            String definition = data.get("d" + i);
            String client = data.get("c" + i);
            if (id == null || folder <= 0 || folder > Integer.MAX_VALUE || message <= 0 || message > Integer.MAX_VALUE
                    || created <= 0 || time < created || definition == null || client == null
                    || result.putIfAbsent(id, new Dam(id, (int) folder, (int) message, definition, client, created, time)) != null) {
                throw ServiceException.FAILURE("invalid deferred action row", null);
            }
        }
        return result;
    }

    private static void save(Mailbox mailbox, OperationContext op, Map<String, Dam> records) throws ServiceException {
        Metadata data = new Metadata();
        data.put("count", records.size());
        int index = 0;
        for (Dam value : records.values()) {
            data.put("id" + index, value.id());
            data.put("f" + index, value.folder());
            data.put("m" + index, value.message());
            data.put("d" + index, value.definition());
            data.put("c" + index, value.client());
            data.put("t" + index, value.time());
            data.put("created" + index, value.created());
            index++;
        }
        mailbox.setConfig(op, SECTION, data);
    }

    @Override
    public void afterDelivery(Account account, Mailbox mailbox, String sender, String recipient, Message delivered) {
        if (!active || delivered == null || Arrays.stream(delivered.getTags()).noneMatch(DeferredActions::isMarker)) {
            return;
        }
        try {
            if (!capture(mailbox, null, List.of(delivered.getId()))) {
                ZimbraLog.lmtp.warn("EWSGo deferred action mailbox queue is full");
            }
        } catch (ServiceException e) {
            ZimbraLog.lmtp.warn("EWSGo deferred action could not capture native delivery");
        }
    }

    private static boolean isMarker(String tag) {
        return MARKER_NAME.matcher(tag).matches();
    }

    private static Message findMessage(Mailbox mailbox, OperationContext op, int id) throws ServiceException {
        try {
            return mailbox.getMessageById(op, id);
        } catch (ServiceException e) {
            if (MailServiceException.NO_SUCH_MSG.equals(e.getCode()) || MailServiceException.NO_SUCH_ITEM.equals(e.getCode())) {
                return null;
            }
            throw e;
        }
    }

    private static boolean capture(Mailbox mailbox, OperationContext op, List<Integer> messages) throws ServiceException {
        mailbox.lock(true);
        try {
            Map<String, Dam> records = read(mailbox, op);
            int count = records.size();
            var recorded = new ArrayList<AttachedMarker>();
            var definitions = new TreeMap<String, String>();
            boolean full = false;
            long now = System.currentTimeMillis();
            for (int message : messages) {
                Message current = findMessage(mailbox, op, message);
                if (current == null) {
                    continue;
                }
                for (String tag : current.getTags()) {
                    if (!isMarker(tag)) {
                        continue;
                    }
                    String marker = tag.substring(MARKER.length());
                    String id = message + "-" + marker;
                    if (!records.containsKey(id)) {
                        if (!definitions.containsKey(marker)) {
                            Metadata definition = mailbox.getConfig(op, DEFINITION + marker);
                            definitions.put(marker, definition == null ? null : definition.get("wire"));
                        }
                        String definition = definitions.get(marker);
                        if (definition == null) {
                            continue;
                        }
                        if (records.size() == MAX_RECORDS) {
                            full = true;
                            continue;
                        }
                        records.put(id, new Dam(id, current.getFolderId(), message, definition, "", now, now));
                    }
                    recorded.add(new AttachedMarker(message, tag));
                }
            }
            if (records.size() != count) {
                save(mailbox, op, records);
            }
            for (AttachedMarker marker : recorded) {
                mailbox.alterTag(op, marker.message(), MailItem.Type.MESSAGE, marker.tag(), false, null);
            }
            return !full;
        } finally {
            mailbox.unlock();
        }
    }

    private static void reconcile(Mailbox mailbox, OperationContext op) throws ServiceException {
        if (read(mailbox, op).size() == MAX_RECORDS) {
            return;
        }
        int remaining = RECOVERY_BATCH;
        for (Tag tag : mailbox.getTagList(op)) {
            String name = tag.getName();
            if (tag.getItemCount() == 0 || !isMarker(name)
                    || mailbox.getConfig(op, DEFINITION + name.substring(MARKER.length())) == null) {
                continue;
            }
            SearchParams params = new SearchParams();
            params.setQueryString("is:anywhere tag:\"" + name + "\"");
            params.setTypes(Set.of(MailItem.Type.MESSAGE));
            params.setSortBy(SortBy.ID_ASC);
            params.setIncludeTagDeleted(true);
            params.setIncludeTagMuted(true);
            params.setLimit(remaining);

            // Native search requires an unlocked mailbox. Close the result set
            // before calling capture, which re-checks each tag under the write lock.
            var candidates = new ArrayList<Integer>();
            try (ZimbraQueryResults hits = mailbox.index.search(op, params)) {
                while (candidates.size() < remaining && hits.hasNext()) {
                    candidates.add(hits.getNext().getItemId());
                }
            } catch (IOException e) {
                throw ServiceException.FAILURE("cannot close deferred action marker search", e);
            }
            remaining -= candidates.size();
            if ((!candidates.isEmpty() && !capture(mailbox, op, candidates)) || remaining == 0) {
                return;
            }
        }
    }

    @Override
    public void forwardWithoutDelivery(Account account, Mailbox mailbox, String sender, String recipient, ParsedMessage message) {
        // A forwarded-only message has no local target.
    }

    private static final class Handler extends DocumentHandler {
        private final String operation;

        Handler(String operation) {
            this.operation = operation;
        }

        @Override
        public Element handle(Element request, Map<String, Object> context) throws ServiceException {
            ZimbraSoapContext soap = getZimbraSoapContext(context);
            Account account = IncomingRulesExtension.owner(soap, this);
            Mailbox mailbox = MailboxManager.getInstance().getMailboxByAccount(account);
            OperationContext op = new OperationContext(account);
            Element response = soap.createElement(QName.get(operation + "Response", NS));
            boolean targets = "ReadDeferredActions".equals(operation) && request.getAttributeBool("targets", false);
            if ("ReadDeferredActions".equals(operation) && !targets && request.getAttributeBool("reconcile", false)) {
                reconcile(mailbox, op);
            }
            mailbox.lock(true);
            try {
                Map<String, Dam> records = read(mailbox, op);
                switch (operation) {
                    case "ReadDeferredActions":
                        if (targets) {
                            Set<Integer> emitted = new HashSet<>();
                            for (Dam row : records.values()) {
                                if (emitted.add(row.message())) {
                                    response.addElement(QName.get("message", NS)).addAttribute("id", row.message());
                                }
                            }
                            break;
                        }
                        for (Dam row : records.values()) {
                            response.addElement(QName.get("action", NS)).addAttribute("id", row.id())
                                    .addAttribute("folder", row.folder()).addAttribute("message", row.message())
                                    .addAttribute("definition", row.definition()).addAttribute("client", row.client())
                                    .addAttribute("created", row.created()).addAttribute("modified", row.time());
                        }
                        break;
                    case "PatchDeferredActions":
                        patch(request, mailbox, op, records);
                        break;
                    case "DeleteDeferredAction":
                        String id = request.getAttribute("id");
                        Dam removed = records.get(id);
                        if (removed != null) {
                            String marker = MARKER + id.substring(id.indexOf('-') + 1);
                            Message message = findMessage(mailbox, op, removed.message());
                            if (message != null && Arrays.asList(message.getTags()).contains(marker)) {
                                mailbox.alterTag(op, removed.message(), MailItem.Type.MESSAGE, marker, false, null);
                            }
                            records.remove(id);
                            save(mailbox, op, records);
                        }
                        break;
                    default:
                        throw ServiceException.INVALID_REQUEST("unknown deferred action operation", null);
                }
                return response.addAttribute("status", "applied");
            } finally {
                mailbox.unlock();
            }
        }

        private static void patch(Element request, Mailbox mailbox, OperationContext op, Map<String, Dam> records)
                throws ServiceException {
            long folder = request.getAttributeLong("folder", 0);
            long message = request.getAttributeLong("message", 0);
            byte[] server = decode(request.getAttribute("server"), MAX_WIRE, false);
            byte[] client = decode(request.getAttribute("client"), MAX_WIRE, false);
            if (folder < 0 || folder > Integer.MAX_VALUE || message < 0 || message > Integer.MAX_VALUE
                    || (folder == 0) != (message == 0) || server.length == 0 || client.length == 0) {
                throw ServiceException.INVALID_REQUEST("invalid deferred action identifiers", null);
            }
            String old = Base64.getEncoder().encodeToString(server);
            String next = Base64.getEncoder().encodeToString(client);
            boolean changed = false;
            long now = System.currentTimeMillis();
            for (var entry : records.entrySet()) {
                Dam row = entry.getValue();
                boolean matches = row.client().isEmpty() ? row.folder() == folder && row.message() == message : row.client().equals(old);
                if (matches && !next.equals(row.client())) {
                    entry.setValue(row.patch(next, now));
                    changed = true;
                }
            }
            if (changed) {
                save(mailbox, op, records);
            }
        }
    }
}
