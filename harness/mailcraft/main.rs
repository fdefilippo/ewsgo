// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

//! Isolated test host for the pinned, unmodified MailCraft EWS runner.
use mailcraft_ews::{Client, HttpExchange, HttpRequest, HttpResponse, Method, TransportError};
use mailcraft_model::sync::{EwsFetchRequest, EwsMailRequest, MessageChange, MessageOutcome};
use serde_json::{json, Value};
use std::{future::Future, io::{Read, Write}, pin::pin, process::{Command, Stdio}, sync::{atomic::{AtomicBool, AtomicUsize, Ordering}, Arc}, task::{Context, Poll, Wake, Waker}};

struct CurrentThread(std::thread::Thread);
impl Wake for CurrentThread { fn wake(self: Arc<Self>) { self.0.unpark(); } }
fn block_on<F: Future>(future: F) -> F::Output {
    let waker = Waker::from(Arc::new(CurrentThread(std::thread::current())));
    let mut cx = Context::from_waker(&waker);
    let mut future = pin!(future);
    loop { match future.as_mut().poll(&mut cx) { Poll::Ready(v) => return v, Poll::Pending => std::thread::park() } }
}
struct Network { script: String, ca: String, count: AtomicUsize }
impl HttpExchange for Network {
    async fn exchange(&self, request: &HttpRequest) -> Result<HttpResponse, TransportError> {
        self.count.fetch_add(1, Ordering::Relaxed);
        let fields: Vec<_> = request.fields().collect();
        let input = json!({"url":request.url(),"fields":fields,"body":request.body(),"limit":request.max_response_bytes(),"ca":self.ca});
        let mut child = Command::new("python3").arg(&self.script).stdin(Stdio::piped()).stdout(Stdio::piped()).stderr(Stdio::null()).spawn().map_err(|_| TransportError::Other)?;
        serde_json::to_writer(child.stdin.take().ok_or(TransportError::Other)?, &input).map_err(|_| TransportError::Other)?;
        let output = child.wait_with_output().map_err(|_| TransportError::Other)?;
        let value: Value = serde_json::from_slice(&output.stdout).map_err(|_| TransportError::Other)?;
        if let Some(error) = value["error"].as_str() { return Err(match error {"tls"=>TransportError::Tls,"limit"=>TransportError::BodyTooLarge,"timeout"=>TransportError::TimedOut,_=>TransportError::Unreachable}); }
        let body: Vec<u8> = serde_json::from_value(value["body"].clone()).map_err(|_| TransportError::Other)?;
        let mut response = HttpResponse::new(value["status"].as_u64().ok_or(TransportError::Other)? as u16, body);
        if let Some(fields) = value["fields"].as_array() { for f in fields { if let (Some(k),Some(v))=(f[0].as_str(),f[1].as_str()) { response=response.with_field(k,v); } } }
        Ok(response)
    }
}
fn fetch(v: &Value) -> Result<bool, ()> {
    let request: EwsFetchRequest = serde_json::from_value(v["request"].clone()).map_err(|_| ())?;
    if request.ids.len() != 1 || request.background || !request.url.starts_with("https://") {
        return Err(());
    }
    let username = if request.username.is_empty() { &request.email } else { &request.username };
    let auth = mailcraft_ews::Secret::basic(username, v["password"].as_str().ok_or(())?).map_err(|_| ())?;
    let client = Client::new(&request.url, auth).map_err(|_| ())?;
    let exchange = Network {
        script: v["transport"].as_str().ok_or(())?.to_owned(),
        ca: v["ca"].as_str().ok_or(())?.to_owned(),
        count: AtomicUsize::new(0),
    };
    let result = block_on(mailcraft_ews::fetch_messages(
        &client, &exchange, &request, &AtomicBool::new(false), &mut |_| false,
    ));
    let ok = result.fetched.len() == 1 && result.errors.is_empty();
    serde_json::to_writer(std::io::stdout(), &json!({
        "ok": ok, "networkRequests": exchange.count.load(Ordering::Relaxed), "fetch": result,
    })).map_err(|_| ())?;
    Ok(ok)
}

fn apply(v: &Value) -> Result<bool, ()> {
    let path = v["store"].as_str().ok_or(())?;
    let command = match v["command"].as_str() {
        Some(command @ ("sync.applyEws" | "sync.applyEwsFetch")) => command,
        _ => return Err(()),
    };
    let pieces = v["pieces"].as_array().ok_or(())?;
    if pieces.is_empty() {
        return Err(());
    }
    let opened = mailcraft_engine::store::open(std::path::Path::new(path)).map_err(|_| ())?;
    if opened.notice().is_some() {
        return Err(());
    }
    let mut session = mailcraft_engine::Session::new(opened.mailbox);
    session.path = Some(path.into());
    mailcraft_engine::cmd::file::after_opening(&mut session);
    for piece in pieces {
        session.run(command, &json!({"result": piece})).map_err(|_| ())?;
    }
    session.run("file.saveAs", &json!({"path": path})).map_err(|_| ())?;
    serde_json::to_writer(std::io::stdout(), &json!({"applied": pieces.len()})).map_err(|_| ())?;
    Ok(true)
}

fn run() -> Result<bool, ()> {
    const MAX_INPUT: u64 = 32 << 20;
    let mut input=String::new(); std::io::stdin().take(MAX_INPUT + 1).read_to_string(&mut input).map_err(|_| ())?;
    if input.len() as u64 > MAX_INPUT { return Err(()); }
    let v: Value=serde_json::from_str(&input).map_err(|_| ())?;
    if v["mode"] == "apply" { return apply(&v); }
    if v["mode"] == "fetch" { return fetch(&v); }
    let request: EwsMailRequest=serde_json::from_value(v["request"].clone()).map_err(|_| ())?;
    let c2 = v["mode"] == "c2";
    if c2 {
        let port = request.url.strip_prefix("https://127.0.0.1:")
            .and_then(|url| url.strip_suffix("/EWS/Exchange.asmx"))
            .and_then(|port| port.parse::<u16>().ok());
        if port.is_none() || v["ca"].as_str().is_none_or(str::is_empty) || !request.places.is_empty() {
            return Err(());
        }
        for send in &request.sends {
            let mime = send.mime.as_ref().ok_or(())?;
            if !mime.subject.starts_with("EWSGo laboratory fixture C2 ") || mime.to.len() != 1
                || mime.to[0].email != request.email || !mime.cc.is_empty() || !mime.bcc.is_empty()
            {
                return Err(());
            }
        }
    } else {
        if !request.sends.is_empty() || !request.drafts.is_empty() || !request.places.is_empty() || !request.changes.is_empty() { return Err(()); }
        if request.messages.iter().any(|change| !matches!(change, MessageChange::Update {read:Some(_),flag:None,categories:None,importance:None,..})) {return Err(());}
    }
    if request.url.starts_with("http:") && (request.email!="lab@example.invalid" || !matches!(v["password"].as_str(),Some("synthetic"|"wrong-synthetic"))) {return Err(());}
    let username=if request.username.is_empty() { &request.email } else { &request.username };
    let auth=mailcraft_ews::Secret::basic(username,v["password"].as_str().ok_or(())?).map_err(|_| ())?;
    let client=Client::new(&request.url,auth).map_err(|_| ())?;
    let exchange=Network {script:v["transport"].as_str().ok_or(())?.to_owned(),ca:v["ca"].as_str().unwrap_or("").to_owned(),count:AtomicUsize::new(0)};
    let mut pieces=Vec::new();
    let ok=if v["mode"]=="probe" { block_on(client.check(&exchange)).is_ok() } else {
        block_on(mailcraft_ews::sync_mail(&client,&exchange,&request,&AtomicBool::new(false),&mut |_| false,&mut |piece| pieces.push(piece)));
        let outcomes:Vec<_>=pieces.iter().flat_map(|piece| &piece.messages).collect();
        let writes_ok=outcomes.len()==request.messages.len() && outcomes.iter().all(|message|matches!(message.outcome,MessageOutcome::Done{..}|MessageOutcome::Gone));
        writes_ok && pieces.last().is_some_and(|p| !p.partial && p.errors.is_empty() && p.blocked.is_empty() && !p.auth_refused && !p.stopped && p.busy_for.is_none() && (c2 || p.whole || request.inbox_pass))
    };
    serde_json::to_writer(std::io::stdout(), &json!({"ok":ok,"networkRequests":exchange.count.load(Ordering::Relaxed),"results":pieces})).map_err(|_| ())?;
    Ok(ok)
}
// Synthetic network fixture only. No user configuration or lab credentials accepted.
fn fixture() -> Result<(), ()> {
    use std::net::TcpListener;
    let server=mailcraft_ews::standin::StandIn::new();
    server.state().authorization=mailcraft_ews::Secret::basic("lab@example.invalid","synthetic").map_err(|_| ())?.expose().to_owned();
    let listener=TcpListener::bind("127.0.0.1:0").map_err(|_| ())?;
    println!("http://{}/EWS/Exchange.asmx",listener.local_addr().map_err(|_| ())?); std::io::stdout().flush().map_err(|_| ())?;
    for stream in listener.incoming() {
        let mut stream=stream.map_err(|_| ())?; stream.set_read_timeout(Some(std::time::Duration::from_secs(10))).map_err(|_| ())?;
        let mut head=Vec::new();
        while !head.ends_with(b"\r\n\r\n") { let mut b=[0];stream.read_exact(&mut b).map_err(|_| ())?;head.push(b[0]);if head.len()>32768 {return Err(());} }
        let text=String::from_utf8(head).map_err(|_| ())?;
        let fields:Vec<(&str,&str)>=text.lines().skip(1).filter_map(|l|l.split_once(':').map(|(k,v)|(k,v.trim()))).collect();
        let size=fields.iter().find(|(k,_)|k.eq_ignore_ascii_case("content-length")).and_then(|(_,v)|v.parse::<usize>().ok()).ok_or(())?;
        if size>2<<20 {return Err(());} let mut body=vec![0;size]; stream.read_exact(&mut body).map_err(|_| ())?;
        let req=HttpRequest::new(Method::Post,"http://127.0.0.1/EWS/Exchange.asmx",&fields,body).map_err(|_| ())?;
        let response=block_on(server.exchange(&req)).map_err(|_| ())?;
        write!(stream,"HTTP/1.1 {} Fixture\r\nContent-Type: text/xml\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",response.status(),response.body().len()).map_err(|_| ())?;
        stream.write_all(response.body()).map_err(|_| ())?;
    }
    Ok(())
}
fn main() { let result=if std::env::args().nth(1).as_deref()==Some("fixture") {fixture().map(|_|true)} else {run()}; if result != Ok(true) {eprintln!("MailCraft harness failed (private details omitted)");std::process::exit(1);} }
