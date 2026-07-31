
use edge_api::api;
// 1. ホストからデータを受け取るためのバッファを確保
const BUF_SIZE: usize = 2048;

#[no_mangle]
pub extern "C" fn on_request() {
    api::init_logger(log::LevelFilter::Info);
    log::info!("[Wasm] on_request hook called");


    let mut buffer = vec![0u8; BUF_SIZE];


    let req = match api::get_request(&mut buffer){
        Ok(req_info) => {
            log::info!("[Wasm] sizeof RequestInfo: {}", std::mem::size_of::<api::Request>());
            log::info!("[Wasm] serialized RequestInfo size: {}", req_info.request_size());
            log::info!("[Wasm] Successfully decoded RequestInfo");
            log::info!("[Wasm] Method: {}", req_info.method());
            // 4. デコードしたデータを使って何らかの処理を行う
            log::info!("[Wasm] Path: {}", req_info.path());
            for (key,value) in req_info.headers() {
                log::info!("[Wasm] Header: {} = {}", key, value);
            }
            match req_info.tls() {
                Some(tls) => log::info!(
                    "[Wasm] TLS: version=0x{:04x} cipher=0x{:04x} sni={} alpn={} client_cert={}B",
                    tls.version(), tls.cipher_suite(), tls.sni(), tls.alpn(), tls.client_cert().len()
                ),
                None => log::info!("[Wasm] TLS: none (plaintext)"),
            }
            req_info
        }
        Err(err) => {
            log::error!("[Wasm] Failed to decode request info: {}", err);
            return;
        }
    };
    let host = req.host();
    let host_without_port = host.split(':').next().unwrap_or(host);
    let (redirect_url,h3_port) = match api::popcache_config() {
        Some(pc) => (format!("https://{}:{}/index.html", host_without_port, pc.https_port), Some(format!("h3=\":{}\"; ma=86400", pc.http3_port))),
        None => (format!("https://{}/index.html", host_without_port), None),   
    };
    let mut cs =   api::ChangeSet::new();
    if let Some(h3) = &h3_port {
        cs.response_field(api::DiffKind::replace, "Alt-Svc", &h3);
    }
    cs.response_field(api::DiffKind::delete, "X-App-Name","");
    cs.response_field(api::DiffKind::delete, "X-App-Version","");
    if req.path() == "/" {      
        cs.request_routing(api::Routing::deny) 
            .status(308) // HTTP 308 Permanent Redirect
            .location(&redirect_url)
            .response_body(b"Redirecting to index.html");
        log::info!("[Wasm] Redirecting to: {}", redirect_url);
        // anyway, we need to save the buffer
    }
    cs.apply().expect("Failed to create request changes");

    let req_size = req.request_size();
    buffer.resize(req_size, 0);
    api::save_buffer(buffer);

}

#[no_mangle]
pub extern "C" fn on_response() {
    let buffer = api::get_buffer();
    if buffer.is_none() {
        log::warn!("[Wasm] No buffer available");
        return;
    }
    let mut buffer = buffer.unwrap();
    log::info!("[Wasm] on_response hook called");
    log::info!("[Wasm] Buffer size: {}", buffer.len());
    let (req_sni, req_ip, req_local, fetch_url) = match api::decode_request(&buffer) {
        Ok(req_info) => {
            log::info!("[Wasm] Protocol: {} {}",req_info.protocol(),if req_info.is_tls() { "(TLS)" } else { "(Plain)" });
            let (lip, lport) = req_info.local_addr();
            log::info!("[Wasm] Local addr: {}:{}", lip, lport);
            let fetch_url = req_info.headers()
                .find(|(k, _)| k.eq_ignore_ascii_case("x-fetch-url"))
                .map(|(_, v)| v.to_string());
            (req_info.tls().map(|t| t.sni().to_string()), Some(req_info.remote_addr().0), format!("{}:{}", lip, lport), fetch_url)
        }
        Err(err) => {
            log::error!("[Wasm] Failed to decode request info: {}", err);
            return;
        }
    };

    // Host callback: geo-locate the client IP.
    let geo_country = req_ip.and_then(api::geoloc).map(|g| {
        log::info!("[Wasm] Geoloc: asn={} org={} country={} city={}", g.asn, g.asn_org, g.country, g.city);
        g.country
    });

    // Host callback: lazily pull the request body and note its length.
    let req_body_len = api::get_request_body().map(|b| b.len()).unwrap_or(0);
    log::info!("[Wasm] Request body: {} bytes", req_body_len);

    buffer.resize(BUF_SIZE, 0);

    match api::get_response(&mut buffer) {
        Ok(resp_info) => {
            log::info!("[Wasm] sizeof ResponseInfo: {}", std::mem::size_of::<api::Response>());
            log::info!("[Wasm] serialized ResponseInfo size: {}", resp_info.response_size());
            log::info!("[Wasm] Successfully decoded ResponseInfo");
            log::info!("[Wasm] Status: {}", resp_info.status());
            for (key, value) in resp_info.headers() {
                log::info!("[Wasm] Header: {} = {}", key, value);
            }
            if resp_info.status() != 200 {
                log::info!("[Wasm] Response status is not 200, skipping further processing");
                return;
            }
        }
        Err(err) => {
            log::error!("[Wasm] Failed to decode response info: {}", err);
        }
    }

    // Host callback: fetch this popcache node's serving config (origin + ports)
    // and echo it, proving the get_popcache_config host callback round-trips.
    let pc = api::popcache_config();
    if let Some(c) = pc.as_ref() {
        log::info!("[Wasm] Popcache config: origin={} http={} https={} http3={}",
            c.origin, c.http_port, c.https_port, c.http3_port);
    }
    let pc_origin = pc.as_ref().map(|c| c.origin.clone()).unwrap_or_default();
    let pc_http3 = pc.as_ref().map(|c| c.http3_port.to_string()).unwrap_or_default();

    // Host callback: outbound fetch. Exercised only when the request carries an
    // X-Fetch-Url header (the e2e test path); echo status + body length, never
    // the body itself. The host's per-module allowed_hosts (default-deny) is
    // what actually authorizes the destination.
    let url = fetch_url.unwrap_or("https://raw.githubusercontent.com/on-keyday/brgen/refs/heads/main/rebrgen/src/ebm/extended_binary_module.bgn".to_string());
    let fetch_result = api::fetch("GET", &url, &[("X-From-Wasm", "1")], b"", 0);
    let fetch_echo = {
        log::info!("[Wasm] Fetching: {}", url);
        match &fetch_result {
            Ok(resp) => {
                log::info!("[Wasm] Fetch OK: status={} body={} bytes headers={}",
                    resp.status, resp.body.len(), resp.headers.len());
                (resp.status.to_string(), String::from_utf8_lossy(&resp.body),false)
            }
            Err(err) => {
                log::warn!("[Wasm] Fetch failed: {}", err);
                ("0".to_string(), String::from_utf8_lossy(err.as_bytes()),true)
            }
        }
    };

    let mut cs = api::ChangeSet::new();
    cs.response_field(api::DiffKind::replace, "X-Wasm-Processed", "true");
    // Echo the request's TLS SNI back so the host side can observe that the
    // TLS read-context field round-tripped through the ABI.
    if let Some(sni) = req_sni.as_deref() {
        if !sni.is_empty() {
            cs.response_field(api::DiffKind::replace, "X-Wasm-Sni", sni);
        }
    }
    // Echo the geo-located country so the host can observe the callback worked.
    if let Some(country) = geo_country.as_deref() {
        if !country.is_empty() {
            cs.response_field(api::DiffKind::replace, "X-Wasm-Geo", country);
        }
    }
    cs.response_field(api::DiffKind::replace, "X-Wasm-Local", &req_local);
    let req_body_len_str = req_body_len.to_string();
    cs.response_field(api::DiffKind::replace, "X-Wasm-ReqBody-Len", &req_body_len_str);
    // Echo the popcache serving config the host reported.
    if !pc_origin.is_empty() {
        // cs.response_field(api::DiffKind::replace, "X-Wasm-Origin", &pc_origin);
        cs.response_field(api::DiffKind::replace, "X-Wasm-Http3-Port", &pc_http3);
    }
    // Echo the fetch outcome (status + body length only — never the body).
    let (status, body, is_error) = fetch_echo;
    let len_or_error = if is_error { body.as_ref() } else { &body.len().to_string() };
    cs.response_field(api::DiffKind::replace, "X-Wasm-Fetch-Status", &status);
    cs.response_field(api::DiffKind::replace, "X-Wasm-Fetch-Result", len_or_error);

    cs.response_field(api::DiffKind::delete, "Content-Length", "");
    cs.append_response_body(b"\nthis comment is added at edge by the wasm module\nand below is EBM schema I developed\n");
    cs.append_response_body(body.as_bytes());
    cs.apply().expect("Failed to apply response changes");
}