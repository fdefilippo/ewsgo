// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package org.ewsgo.zimbra;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.util.Locale;
import java.util.Map;
import java.util.TreeMap;

import javax.mail.MessagingException;
import javax.mail.internet.ContentType;

import org.dom4j.QName;

import com.zimbra.common.service.ServiceException;
import com.zimbra.common.soap.Element;
import com.zimbra.common.util.ZimbraLog;
import com.zimbra.cs.account.Account;
import com.zimbra.cs.lmtpserver.LmtpCallback;
import com.zimbra.cs.mailbox.Folder;
import com.zimbra.cs.mailbox.MailItem;
import com.zimbra.cs.mailbox.Mailbox;
import com.zimbra.cs.mailbox.MailboxManager;
import com.zimbra.cs.mailbox.Message;
import com.zimbra.cs.mailbox.Metadata;
import com.zimbra.cs.mailbox.OperationContext;
import com.zimbra.cs.mime.ParsedMessage;
import com.zimbra.soap.DocumentDispatcher;
import com.zimbra.soap.DocumentHandler;
import com.zimbra.soap.ZimbraSoapContext;

final class ReceiveRouting implements LmtpCallback {
    private static final String NS = IncomingRulesExtension.NS;
    private static final QName READ = QName.get("ReadReceiveFoldersRequest", NS);
    private static final QName SET = QName.get("SetReceiveFolderRequest", NS);
    private static final String SECTION = "ewsgo.mapi.receive-folders.v1";
    private static final int MAX_ROUTES = 64;
    private volatile boolean active;

    void setActive(boolean enabled) {
        active = enabled;
    }

    void register(DocumentDispatcher dispatcher) {
        dispatcher.registerHandler(READ, new RoutingHandler(RoutingOperation.READ));
        dispatcher.registerHandler(SET, new RoutingHandler(RoutingOperation.SET));
    }

    void deregister(DocumentDispatcher dispatcher) {
        dispatcher.unRegisterHandler(READ);
        dispatcher.unRegisterHandler(SET);
    }

    private record Route(int folder, long modified) {}

    private static boolean validClass(String cls) {
        return cls.length() <= 254 && !cls.startsWith(".") && !cls.endsWith(".") && !cls.contains("..")
                && cls.chars().allMatch(ch -> ch >= 32 && ch <= 126);
    }

    private static Map<String, Route> read(Mailbox mailbox, OperationContext op) throws ServiceException {
        Map<String, Route> routes = new TreeMap<>();
        Metadata data = mailbox.getConfig(op, SECTION);
        if (data == null) {
            return routes;
        }
        long count = data.getLong("count", -1L);
        if (count < 0 || count > MAX_ROUTES) {
            throw ServiceException.FAILURE("invalid receive routing metadata", null);
        }
        for (int i = 0; i < count; i++) {
            String cls = data.get("c" + i);
            long id = data.getLong("f" + i, -1L);
            long modified = data.getLong("t" + i, 0L);
            if (cls == null || !validClass(cls) || id <= 0 || id > Integer.MAX_VALUE || modified <= 0
                    || routes.putIfAbsent(cls, new Route((int) id, modified)) != null) {
                throw ServiceException.FAILURE("invalid receive routing row", null);
            }
        }
        return routes;
    }

    private static void save(Mailbox mailbox, OperationContext op, Map<String, Route> routes) throws ServiceException {
        Metadata data = new Metadata();
        data.put("count", routes.size());
        int index = 0;
        for (var row : routes.entrySet()) {
            data.put("c" + index, row.getKey());
            data.put("f" + index, row.getValue().folder());
            data.put("t" + index, row.getValue().modified());
            index++;
        }
        mailbox.setConfig(op, SECTION, data);
    }

    private static boolean isDestination(Mailbox mailbox, OperationContext op, int id) throws ServiceException {
        Folder target = mailbox.getFolderById(op, id);
        if (target.getType() != MailItem.Type.FOLDER || target.getDefaultView() != MailItem.Type.MESSAGE) {
            return false;
        }
        Folder parent = target;
        for (int depth = 0; depth < 256; depth++) {
            if (parent.getId() == Mailbox.ID_FOLDER_TRASH || parent.getId() == Mailbox.ID_FOLDER_SPAM) {
                return false;
            }
            if (parent.getId() == Mailbox.ID_FOLDER_USER_ROOT) {
                return parent != target;
            }
            if (parent.getFolderId() <= 0 || parent.getFolderId() == parent.getId()) {
                return false;
            }
            parent = mailbox.getFolderById(op, parent.getFolderId());
        }
        return false;
    }

    @Override
    public void afterDelivery(Account account, Mailbox mailbox, String sender, String recipient, Message delivered) {
        if (!active || delivered.getFolderId() != Mailbox.ID_FOLDER_INBOX) {
            return;
        }
        mailbox.lock(true);
        try {
            Map<String, Route> routes = read(mailbox, null);
            if (routes.isEmpty()) {
                return;
            }
            Message current = mailbox.getMessageById(null, delivered.getId());
            if (current.getFolderId() != Mailbox.ID_FOLDER_INBOX) {
                return;
            }
            String cls = messageClass(current);
            Route selected = null;
            int longest = cls.startsWith("REPORT.IPM") ? 10 : 3;
            for (var entry : routes.entrySet()) {
                String prefix = entry.getKey();
                // The two built-in parents protect normal fallback to Inbox.
                if (prefix.length() > longest && cls.startsWith(prefix)) {
                    selected = entry.getValue();
                    longest = prefix.length();
                }
            }
            if (selected != null && selected.folder() != Mailbox.ID_FOLDER_INBOX
                    && isDestination(mailbox, null, selected.folder())) {
                mailbox.move(null, current.getId(), MailItem.Type.MESSAGE, selected.folder());
            }
        } catch (ServiceException | MessagingException | IOException e) {
            // Delivery already committed. Leave the original in Inbox if routing fails.
            ZimbraLog.lmtp.warn("EWSGo receive routing could not apply the mailbox association");
        } finally {
            mailbox.unlock();
        }
    }

    private static String messageClass(Message message) throws ServiceException, MessagingException, IOException {
        var mime = message.getMimeMessage(false);
        ContentType type = new ContentType(mime.getContentType());
        if (type.match("multipart/report")) {
            String report = type.getParameter("report-type");
            if ("delivery-status".equalsIgnoreCase(report)) {
                var multipart = mime.getContent();
                if (multipart instanceof javax.mail.Multipart parts && parts.getCount() >= 2) {
                    var notification = parts.getBodyPart(1);
                    if (notification.isMimeType("message/delivery-status")) {
                        return deliveryClass(notification);
                    }
                }
            }
        }
        return "IPM.NOTE";
    }

    private static String deliveryClass(javax.mail.BodyPart notification) throws MessagingException, IOException {
        try (var stream = notification.getInputStream()) {
            byte[] bytes = stream.readNBytes(65537);
            if (bytes.length <= 65536) {
                boolean delivered = false;
                for (String line : new String(bytes, StandardCharsets.US_ASCII).split("\\r?\\n")) {
                    if (line.regionMatches(true, 0, "Action:", 0, 7)) {
                        String action = line.substring(7).strip();
                        if ("failed".equalsIgnoreCase(action)) {
                            return "REPORT.IPM.NOTE.NDR";
                        }
                        delivered |= "delivered".equalsIgnoreCase(action);
                    }
                }
                if (delivered) {
                    return "REPORT.IPM.NOTE.DR";
                }
            }
        }
        return "REPORT.IPM.NOTE";
    }

    @Override
    public void forwardWithoutDelivery(Account account, Mailbox mailbox, String sender, String recipient, ParsedMessage message) {
        // There is no local message to move.
    }

    private enum RoutingOperation { READ, SET }

    private static final class RoutingHandler extends DocumentHandler {
        private final RoutingOperation operation;

        RoutingHandler(RoutingOperation operation) {
            this.operation = operation;
        }

        @Override
        public Element handle(Element request, Map<String, Object> context) throws ServiceException {
            ZimbraSoapContext soap = getZimbraSoapContext(context);
            Account account = IncomingRulesExtension.owner(soap, this);
            Mailbox mailbox = MailboxManager.getInstance().getMailboxByAccount(account);
            OperationContext op = new OperationContext(account);
            mailbox.lock(true);
            try {
                Map<String, Route> routes = read(mailbox, op);
                if (operation == RoutingOperation.SET) {
                    return change(request, soap, mailbox, op, routes);
                }
                Element response = soap.createElement(QName.get("ReadReceiveFoldersResponse", NS));
                for (var row : routes.entrySet()) {
                    Route value = row.getValue();
                    try {
                        if (isDestination(mailbox, op, value.folder())) {
                            response.addElement(QName.get("route", NS)).addAttribute("class", row.getKey())
                                    .addAttribute("folder", value.folder()).addAttribute("modified", value.modified());
                        }
                    } catch (ServiceException e) {
                        if (!"mail.NO_SUCH_FOLDER".equals(e.getCode()) && !"mail.NO_SUCH_ITEM".equals(e.getCode())) {
                            throw e;
                        }
                    }
                }
                return response;
            } finally {
                mailbox.unlock();
            }
        }

        private static Element change(Element request, ZimbraSoapContext soap, Mailbox mailbox, OperationContext op,
                                      Map<String, Route> routes) throws ServiceException {
            String cls = request.getAttribute("class");
            long id = request.getAttributeLong("folder");
            if (!validClass(cls) || id < 0 || id > Integer.MAX_VALUE || id == 0 && cls.isEmpty()) {
                throw ServiceException.INVALID_REQUEST("invalid receive association", null);
            }
            cls = cls.toUpperCase(Locale.ROOT);
            if (cls.equals("IPM") || cls.equals("REPORT.IPM")) {
                throw ServiceException.PERM_DENIED("protected receive association");
            }
            if (id == 0) {
                if (routes.remove(cls) != null) {
                    save(mailbox, op, routes);
                }
            } else {
                if (!isDestination(mailbox, op, (int) id)) {
                    throw ServiceException.INVALID_REQUEST("invalid receive destination", null);
                }
                if (!routes.containsKey(cls) && routes.size() == MAX_ROUTES) {
                    throw ServiceException.INVALID_REQUEST("too many receive associations", null);
                }
                routes.put(cls, new Route((int) id, System.currentTimeMillis()));
                save(mailbox, op, routes);
            }
            return soap.createElement(QName.get("SetReceiveFolderResponse", NS)).addAttribute("status", "applied");
        }
    }
}
