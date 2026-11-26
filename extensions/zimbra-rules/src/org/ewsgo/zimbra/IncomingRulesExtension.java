// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package org.ewsgo.zimbra;

import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.util.Base64;
import java.util.HashSet;
import java.util.HexFormat;
import java.util.List;
import java.util.Map;

import org.apache.jsieve.exception.SieveException;
import org.apache.jsieve.parser.generated.ParseException;
import org.apache.jsieve.parser.generated.TokenMgrError;
import org.dom4j.QName;

import com.zimbra.common.service.ServiceException;
import com.zimbra.common.soap.Element;
import com.zimbra.common.soap.MailConstants;
import com.zimbra.cs.account.Account;
import com.zimbra.cs.account.AuthToken;
import com.zimbra.cs.account.Provisioning;
import com.zimbra.cs.account.ldap.entry.LdapAccount;
import com.zimbra.cs.extension.ZimbraExtension;
import com.zimbra.cs.filter.DummyMailAdapter;
import com.zimbra.cs.filter.RuleManager;
import com.zimbra.cs.filter.SieveToSoap;
import com.zimbra.cs.filter.SoapToSieve;
import com.zimbra.cs.ldap.LdapClient;
import com.zimbra.cs.ldap.LdapServerType;
import com.zimbra.cs.ldap.LdapUsage;
import com.zimbra.cs.ldap.ZAttributes;
import com.zimbra.cs.ldap.ZLdapContext;
import com.zimbra.cs.ldap.ZLdapFilterFactory;
import com.zimbra.cs.ldap.ZModificationList;
import com.zimbra.cs.lmtpserver.ZimbraLmtpBackend;
import com.zimbra.soap.DocumentDispatcher;
import com.zimbra.soap.DocumentHandler;
import com.zimbra.soap.RemovableDocumentService;
import com.zimbra.soap.SoapServlet;
import com.zimbra.soap.ZimbraSoapContext;
import com.zimbra.soap.mail.message.GetFilterRulesResponse;
import com.zimbra.soap.mail.message.ModifyFilterRulesRequest;
import com.zimbra.soap.mail.type.FilterRule;

public final class IncomingRulesExtension implements ZimbraExtension, RemovableDocumentService {
    static final String NS = "urn:ewsgo:zimbra:rules:1";
    private static final String SERVLET = "SoapServlet";
    private static final QName READ = QName.get("ReadIncomingRulesRequest", NS);
    private static final QName REPLACE = QName.get("ReplaceIncomingRulesRequest", NS);
    private static final String SCRIPT = Provisioning.A_zimbraMailSieveScript;
    private static final String CSN = "entryCSN";
    private static final int MAX_BYTES = 131072;
    private final ReceiveRouting receiveRouting = new ReceiveRouting();
    private final DeferredActions deferredActions = new DeferredActions();

    @Override
    public String getName() {
        return "ewsgo-rules";
    }

    @Override
    public void init() {
        ZimbraLmtpBackend.addCallback(receiveRouting);
        receiveRouting.setActive(true);
        ZimbraLmtpBackend.addCallback(deferredActions);
        deferredActions.setActive(true);
        SoapServlet.addService(SERVLET, this);
    }

    @Override
    public void destroy() {
        receiveRouting.setActive(false);
        deferredActions.setActive(false);
        SoapServlet.removeService(SERVLET, this);
    }

    @Override
    public void registerHandlers(DocumentDispatcher dispatcher) {
        dispatcher.registerHandler(READ, new RulesHandler(false));
        dispatcher.registerHandler(REPLACE, new RulesHandler(true));
        receiveRouting.register(dispatcher);
        deferredActions.register(dispatcher);
    }

    @Override
    public void deregisterHandlers(DocumentDispatcher dispatcher) {
        dispatcher.unRegisterHandler(READ);
        dispatcher.unRegisterHandler(REPLACE);
        receiveRouting.deregister(dispatcher);
        deferredActions.deregister(dispatcher);
    }

    static Account owner(ZimbraSoapContext soap, DocumentHandler handler) throws ServiceException {
        AuthToken token = soap.getAuthToken();
        if (token == null || !token.isZimbraUser() || AuthToken.isAnyAdmin(token)
                || token.isDelegatedAuth() || soap.isDelegatedRequest()) {
            throw ServiceException.PERM_DENIED("mailbox owner required");
        }
        Account account = DocumentHandler.getAuthenticatedAccount(soap);
        if (!account.getId().equals(soap.getRequestedAccountId()) || !Provisioning.onLocalServer(account)
                || !handler.canModifyOptions(soap, account)) {
            throw ServiceException.PERM_DENIED("mailbox owner required");
        }
        return account;
    }

    private record Snapshot(String csn, String script) {
        byte[] bytes() {
            return script.getBytes(StandardCharsets.UTF_8);
        }

        String revision() {
            try {
                MessageDigest digest = MessageDigest.getInstance("SHA-256");
                digest.update(csn.getBytes(StandardCharsets.UTF_8));
                digest.update((byte) 0);
                return HexFormat.of().formatHex(digest.digest(bytes()));
            } catch (NoSuchAlgorithmException e) {
                throw new IllegalStateException("SHA-256 unavailable", e);
            }
        }
    }

    private static Snapshot read(ZLdapContext ldap, String dn) throws ServiceException {
        ZAttributes attrs = ldap.getAttributes(dn, new String[]{SCRIPT, CSN});
        String script = attrs.getAttrString(SCRIPT);
        String csn = attrs.getAttrString(CSN);
        if (csn == null || csn.isEmpty()) {
            throw ServiceException.FAILURE("incoming rule revision unavailable", null);
        }
        Snapshot snapshot = new Snapshot(csn, script == null ? "" : script);
        if (snapshot.bytes().length > MAX_BYTES) {
            throw ServiceException.INVALID_REQUEST("incoming rule script too large", null);
        }
        return snapshot;
    }

    private static List<FilterRule> losslessProjection(String script) {
        if (script.isEmpty()) {
            return List.of();
        }
        try {
            SieveToSoap converter = new SieveToSoap(RuleManager.getRuleNames(script));
            converter.accept(RuleManager.parse(script));
            List<FilterRule> rules = converter.toFilterRules();
            // A conversion is published only if it accounts for the whole script.
            String rebuilt = new SoapToSieve(rules).getSieveScript();
            return rebuilt.equals(script) ? rules : null;
        } catch (ParseException | TokenMgrError | ServiceException | RuntimeException e) {
            return null;
        }
    }

    // JAXB can silently discard unknown nodes. Refuse a write if the native
    // model lost a field before validating and rendering the new script.
    private static boolean retainedInput(Element submitted, Element model, int depth) {
        if (depth > 24 || !submitted.getQName().equals(model.getQName())
                || !submitted.getTextTrim().equals(model.getTextTrim())) {
            return false;
        }
        for (Element.Attribute attr : submitted.listAttributes()) {
            if (!attr.getValue().equals(model.getAttribute(attr.getKey(), null))) {
                return false;
            }
        }
        List<Element> input = submitted.listElements();
        List<Element> parsed = model.listElements();
        if (input.size() != parsed.size()) {
            return false;
        }
        for (int i = 0; i < input.size(); i++) {
            if (!retainedInput(input.get(i), parsed.get(i), depth + 1)) {
                return false;
            }
        }
        return true;
    }

    private static final class RulesHandler extends DocumentHandler {
        private final boolean replace;

        RulesHandler(boolean replace) {
            this.replace = replace;
        }

        @Override
        public Element handle(Element request, Map<String, Object> context) throws ServiceException {
            ZimbraSoapContext soap = getZimbraSoapContext(context);
            Account account = owner(soap, this);
            if (!(account instanceof LdapAccount ldapAccount)) {
                throw ServiceException.PERM_DENIED("owner rules only");
            }
            String dn = ldapAccount.getDN();
            ZLdapContext ldap = LdapClient.getContext(LdapServerType.MASTER, LdapUsage.MODIFY_ACCOUNT);
            try {
                Snapshot current = read(ldap, dn);
                if (!replace) {
                    Element response = soap.createElement(QName.get("ReadIncomingRulesResponse", NS));
                    response.addAttribute("revision", current.revision());
                    response.addElement(QName.get("script", NS)).setText(Base64.getEncoder().encodeToString(current.bytes()));
                    List<FilterRule> rules = losslessProjection(current.script());
                    if (rules != null) {
                        GetFilterRulesResponse structured = new GetFilterRulesResponse();
                        structured.setFilterRules(rules);
                        response.addElement(soap.jaxbToElement(structured));
                    }
                    return response;
                }
                return replace(request, soap, ldap, ldapAccount, current);
            } finally {
                LdapClient.closeContext(ldap);
            }
        }

        private Element replace(Element request, ZimbraSoapContext soap, ZLdapContext ldap,
                                LdapAccount account, Snapshot current) throws ServiceException {
            Element response = soap.createElement(QName.get("ReplaceIncomingRulesResponse", NS));
            if (!current.revision().equals(request.getAttribute("expected"))) {
                return response.addAttribute("status", "conflict");
            }
            if (losslessProjection(current.script()) == null) {
                return response.addAttribute("status", "unsupported");
            }
            Element rulesNode = request.getElement(MailConstants.MODIFY_FILTER_RULES_REQUEST.getName());
            if (request.listElements().size() != 1 || !MailConstants.MODIFY_FILTER_RULES_REQUEST.equals(rulesNode.getQName())) {
                throw ServiceException.INVALID_REQUEST("expected native filter rules", null);
            }
            ModifyFilterRulesRequest update = soap.elementToJaxb(rulesNode);
            if (!retainedInput(rulesNode, soap.jaxbToElement(update), 0)) {
                return response.addAttribute("status", "unsupported");
            }
            List<FilterRule> rules = update.getFilterRules();
            var names = new HashSet<String>();
            for (FilterRule rule : rules) {
                String name = rule.getName();
                if (name == null || name.isBlank() || name.codePoints().anyMatch(Character::isISOControl)
                        || !names.add(name)) {
                    throw ServiceException.INVALID_REQUEST("invalid incoming rule name", null);
                }
            }
            if (!checkForwardFilterAttr(account, rules, "incomingFilterRule")) {
                throw ServiceException.PERM_DENIED("filter forwarding is disabled");
            }
            String script = rules.isEmpty() ? "" : new SoapToSieve(rules).getSieveScript();
            if (script.equals(current.script())) {
                return response.addAttribute("status", "applied");
            }
            int size = script.getBytes(StandardCharsets.UTF_8).length;
            long configured = account.getLongAttr(Provisioning.A_zimbraMailSieveScriptMaxSize, 0);
            if (size > MAX_BYTES || configured > 0 && size > configured && size > current.bytes().length) {
                throw ServiceException.INVALID_REQUEST("incoming rule script too large", null);
            }
            try {
                RuleManager.getSieveFactory().evaluate(new DummyMailAdapter(), RuleManager.parse(script));
            } catch (ParseException | TokenMgrError | SieveException e) {
                throw ServiceException.INVALID_REQUEST("invalid incoming rule script", null);
            }
            DeferredActions.rememberRules(account, rules);
            ZModificationList changes = ldap.createModificationList();
            changes.modifyAttr(SCRIPT, script, account, false, false);
            ZLdapFilterFactory filters = ZLdapFilterFactory.getInstance();
            String assertion = "(&(" + CSN + "=" + filters.encodeValue(current.csn()) + ")(zimbraId="
                    + filters.encodeValue(account.getId()) + "))";
            if (!ldap.testAndModifyAttributes(account.getDN(), changes,
                    filters.fromFilterString(ZLdapFilterFactory.FilterId.ADMIN_SEARCH, assertion))) {
                return response.addAttribute("status", "conflict");
            }
            // Preserve the normal filter callback's cache behavior after the atomic write.
            Provisioning.getInstance().reload(account);
            RuleManager.clearCachedRules(account);
            return response.addAttribute("status", "applied");
        }
    }
}
