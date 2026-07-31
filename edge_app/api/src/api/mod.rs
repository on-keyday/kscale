pub mod edge;


#[link(wasm_import_module = "kscale")]
extern "C" {
    // get_request_info(pointer: u32, size: u32) -> u32
    fn get_request_info(pointer: *mut u8, size: u32) -> u32;
    // get_response_info(pointer: u32, size: u32) -> u32
    fn get_response_info(pointer: *mut u8, size: u32) -> u32;
    fn log_output(level: u32, pointer: *const u8, size: u32) -> u32;
    fn save_buffer_pointer(pointer: *const (), size: u32);
    fn get_buffer_pointer(pointer: *mut *const u8,size: *mut u32);

    fn change_request_info(pointer: *const u8, size: u32) -> u32;
    fn change_response_info(pointer: *const u8, size: u32) -> u32;

    // geoloc_lookup(in_ptr, in_size, out_ptr, out_size) -> written_size (0 = none)
    fn geoloc_lookup(in_ptr: *const u8, in_size: u32, out_ptr: *mut u8, out_size: u32) -> u32;

    // get_popcache_config(out_ptr, out_size) -> written_size (0 = unavailable /
    // buffer too small). Input-free: the config is node state, not a query.
    fn get_popcache_config(out_ptr: *mut u8, out_size: u32) -> u32;

    // get_{request,response}_body(out_ptr, out_size) -> total_body_size. Writes
    // min(out_size, total) bytes; a total > out_size means "call again with a
    // buffer of that size". 0 = no body / unavailable.
    #[link_name = "get_request_body"]
    fn host_get_request_body(out_ptr: *mut u8, out_size: u32) -> u32;
    #[link_name = "get_response_body"]
    fn host_get_response_body(out_ptr: *mut u8, out_size: u32) -> u32;

    // fetch(req_ptr, req_size) -> fetch_id. Issues an outbound HTTP(S)
    // sub-request (encoded FetchRequest), gated by the module's registered
    // allowed_hosts (default-deny). 0 = rejected (bad encoding, scheme,
    // allowlist, or per-request budget); transport failures still return an id
    // whose FetchResponseInfo carries status 0 + error. The pull calls use the
    // same convention as get_{request,response}_body.
    #[link_name = "fetch"]
    fn host_fetch(req_ptr: *const u8, req_size: u32) -> u32;
    fn get_fetch_response(id: u32, out_ptr: *mut u8, out_size: u32) -> u32;
    fn get_fetch_body(id: u32, out_ptr: *mut u8, out_size: u32) -> u32;
}

// 1. 独自のロガー構造体を定義
struct CustomLogger;

// 2. `log::Log` トレイトを実装して、カスタム処理を記述
impl log::Log for CustomLogger {
    fn enabled(&self, metadata: &log::Metadata) -> bool {
        // ここではInfoレベル以上のログを有効にする
        metadata.level() <= log::Level::Info
    }

    fn log(&self, record: &log::Record) {
        if self.enabled(record.metadata()) {
            // ★★★ ここがカスタム関数にあたる部分 ★★★
            // 本来はGUIへの描画やファイル書き込みなどを行う
            let message = format!("[edge_app] {}: {}", record.level(), record.args());
            let level = match record.level() {
                log::Level::Debug => edge::LogLevel::DEBUG,
                log::Level::Info => edge::LogLevel::INFO,
                log::Level::Warn => edge::LogLevel::WARN,
                log::Level::Error => edge::LogLevel::ERROR,
                log::Level::Trace => edge::LogLevel::TRACE,
            };
            unsafe {
                let level_value :u8 = level.into();
                log_output(level_value as u32, message.as_ptr(), message.len() as u32);
            }
        }
    }

    fn flush(&self) {}
}

pub fn init_logger(level: log::LevelFilter) {
    // 3. ロガーを初期化
    log::set_boxed_logger(Box::new(CustomLogger)).expect("Failed to set custom logger");
    log::set_max_level(level);
}

pub fn save_buffer(mut buffer: Vec<u8>) {
    if buffer.is_empty() {
        log::warn!("[Wasm] Attempted to set an empty buffer");
        return;
    }
    buffer.shrink_to_fit(); // 不要な容量を削除
    let buffer = buffer.leak();
    unsafe {
        save_buffer_pointer(buffer.as_ptr() as *const (), buffer.len() as u32);
    }
}

pub fn get_buffer() -> Option<Vec<u8>> {
    let mut pointer: *const u8 = std::ptr::null();
    let mut size: u32 = 0;
    unsafe {
        get_buffer_pointer(&mut pointer, &mut size);
    }
    if pointer.is_null() || size == 0 {
        None
    } else {
        unsafe {
            save_buffer_pointer(std::ptr::null(), 0); // バッファをクリア
        }
        Some(unsafe {Vec::from_raw_parts(pointer as *mut u8, size as usize, size as usize)})
    }
}

pub struct Request<'a> {
    info :edge::RequestInfo<'a>,
    request_size: usize,
}

fn addr_port(ap: &edge::AddrPort<'_>) -> (std::net::IpAddr, u16) {
    let ip: std::net::IpAddr = if ap.addr.is_v6() {
        (*ap.addr.addr_v6().unwrap_or(&[0; 16])).into()
    } else {
        (*ap.addr.addr_v4().unwrap_or(&[0; 4])).into()
    };
    (ip, ap.port)
}

fn loosy_or_unknown(data: &[u8]) -> &str {
    match String::from_utf8_lossy(data) {
        std::borrow::Cow::Borrowed(s) => s,
        _ => "UNKNOWN",
    }
}

pub type Routing = edge::Routing;
pub type DiffKind = edge::DiffKind;

impl Request<'_> {
    pub fn method(&self) -> &str {
        if self.info.method.method == edge::Method::OTHER {
            loosy_or_unknown(self.info.method.method_name().unwrap())
        } else {
            let method: Option<&str> = self.info.method.method.into();
            method.unwrap_or("UNKNOWN")
        }
    }

    pub fn path(&self) -> &str {
        loosy_or_unknown(&self.info.path.path.data)
    }

    pub fn headers(&self) -> impl std::iter::Iterator<Item = (&str, &str)> + '_ {
        self.info.header.fields.iter().map(|field| {
            (
                loosy_or_unknown(&field.key.data),
                loosy_or_unknown(&field.value.data),
            )
        })
    }

    pub fn host(&self) -> &str {
        self.headers()
            .find(|(key, _)| key.eq_ignore_ascii_case("host"))
            .map_or("UNKNOWN", |(_, value)| value)
    }

    pub fn protocol(&self) -> &str {
        match self.info.protocol.protocol {
            edge::Protocol::http1_plain | edge::Protocol::http1 => "HTTP/1.1",
            edge::Protocol::h2 => "HTTP/2",
            edge::Protocol::h3 => "HTTP/3",
            edge::Protocol::other => loosy_or_unknown(&self.info.protocol.protocol_name().unwrap()),
            _ => "UNKNOWN",
        }
    }

    /// Client (peer) address and port.
    pub fn remote_addr(&self) -> (std::net::IpAddr, u16) {
        addr_port(&self.info.remote)
    }

    /// Local (server-side) address and port the request landed on. Zero when the
    /// host did not record it (e.g. synthetic requests).
    pub fn local_addr(&self) -> (std::net::IpAddr, u16) {
        addr_port(&self.info.local)
    }

    pub fn request_size(&self) -> usize {
        self.request_size
    }

    pub fn is_tls(&self) -> bool {
        self.info.protocol.protocol == edge::Protocol::http1 ||
        self.info.protocol.protocol == edge::Protocol::h2 ||
        self.info.protocol.protocol == edge::Protocol::h3
    }

    /// TLS handshake details, present when the connection was terminated over
    /// TLS at the edge (None for plaintext).
    pub fn tls(&self) -> Option<Tls<'_>> {
        self.info.tls().map(|info| Tls { info })
    }

}

pub struct Tls<'a> {
    info: &'a edge::TlsInfo<'a>,
}

impl Tls<'_> {
    /// Raw TLS version (e.g. 0x0303 = TLS 1.2, 0x0304 = TLS 1.3).
    pub fn version(&self) -> u16 {
        self.info.version
    }
    /// Negotiated cipher suite id.
    pub fn cipher_suite(&self) -> u16 {
        self.info.cipher_suite
    }
    /// Server Name Indication from the ClientHello ("" if none).
    pub fn sni(&self) -> &str {
        loosy_or_unknown(&self.info.sni.data)
    }
    /// Negotiated ALPN protocol ("" if none).
    pub fn alpn(&self) -> &str {
        loosy_or_unknown(&self.info.alpn.data)
    }
    /// Client leaf certificate DER (empty unless mTLS presented one).
    pub fn client_cert(&self) -> &[u8] {
        &self.info.client_cert
    }
}

pub struct Response<'a> {
    info: edge::ResponseInfo<'a>,
    response_size: usize,
}

impl Response<'_> {
    pub fn status(&self) -> u16 {
        self.info.status
    }

    pub fn headers(&self) -> impl std::iter::Iterator<Item = (&str, &str)> + '_ {
        self.info.header.fields.iter().map(|field| {
            (
                loosy_or_unknown(&field.key.data),
                loosy_or_unknown(&field.value.data),
            )
        })
    }

    pub fn response_size(&self) -> usize {
        self.response_size
    }
}

#[derive(Debug)]
pub enum Error {
    NotEnoughBuffer,
    DecodeError(edge::Error),
    ApplyChangeFailed(bool),
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Error::NotEnoughBuffer => write!(f, "Not enough buffer space"),
            Error::DecodeError(e) => write!(f, "Decode error: {}", e),
            Error::ApplyChangeFailed(is_request) => write!(f, "Failed to apply changes to {}", if *is_request { "request" } else { "response" }),
        }
    }
}   

impl std::error::Error for Error {}

impl From<edge::Error> for Error {
    fn from(e: edge::Error) -> Self {
        Error::DecodeError(e)
    }
}

pub fn decode_request<'a>(buffer: &'a [u8]) -> Result<Request<'a>, Error> {
    match edge::RequestInfo::decode_exact_direct(buffer) {
        Ok(req_info) => {
            Ok(Request { info: req_info, request_size: buffer.len() })
        }
        Err(e) => {
            Err(Error::DecodeError(e))
        }
    }
}

pub fn decode_response<'a>(buffer: &'a [u8]) -> Result<Response<'a>, Error> {
    match edge::ResponseInfo::decode_exact_direct(buffer) {
        Ok(resp_info) => {
            Ok(Response { info: resp_info, response_size: buffer.len() })
        }
        Err(e) => {
            Err(Error::DecodeError(e))
        }
    }
}

pub fn get_request_buffer<'a>(buffer: &'a mut [u8]) -> Result<&'a [u8], Error> {
    let written_size = unsafe {
        get_request_info(buffer.as_mut_ptr(), buffer.len() as u32)
    };
    if written_size == 0 {
        return Err(Error::NotEnoughBuffer);
    }
    Ok(&buffer[..written_size as usize])
}

pub fn get_request<'a>(buffer :&'a mut [u8]) -> Result<Request<'a>,Error> {
    let buffer = get_request_buffer(buffer)?;
    decode_request(&buffer)
}

pub fn get_response_buffer<'a>(buffer: &'a mut [u8]) -> Result<&'a [u8], Error> {
    let written_size = unsafe {
        get_response_info(buffer.as_mut_ptr(), buffer.len() as u32)
    };
    if written_size == 0 {
        return Err(Error::NotEnoughBuffer);
    }
    Ok(&buffer[..written_size as usize])
}

pub fn get_response<'a>(buffer: &'a mut [u8]) -> Result<Response<'a>, Error> {
    let buffer = get_response_buffer(buffer)?;
    decode_response(&buffer)
}

fn pull_body(call: impl Fn(*mut u8, u32) -> u32) -> Option<Vec<u8>> {
    // First call with a modest buffer; the host returns the total size and fills
    // min(total, buf). If the buffer was too small, re-allocate to the exact
    // size and pull again (the host has the body cached, so this is cheap).
    let mut buf = vec![0u8; 4096];
    let total = call(buf.as_mut_ptr(), buf.len() as u32) as usize;
    if total == 0 {
        return None;
    }
    if total > buf.len() {
        buf = vec![0u8; total];
        let n = call(buf.as_mut_ptr(), buf.len() as u32) as usize;
        buf.truncate(n.min(total));
    } else {
        buf.truncate(total);
    }
    Some(buf)
}

/// Fetch the request body from the host (lazy pull; the host buffers it only on
/// demand and caps the size). None means no body, or a body too large to expose.
pub fn get_request_body() -> Option<Vec<u8>> {
    pull_body(|ptr, len| unsafe { host_get_request_body(ptr, len) })
}

/// Fetch the response body from the host (lazy pull). See get_request_body.
pub fn get_response_body() -> Option<Vec<u8>> {
    pull_body(|ptr, len| unsafe { host_get_response_body(ptr, len) })
}

/// Geo-location of an IP as resolved by the host.
pub struct Geoloc {
    pub asn: u32,
    pub asn_org: std::string::String,
    pub country: std::string::String,
    pub city: std::string::String,
}

/// Resolve an IP to its geo-location via the host. Returns None when the host
/// has no locator configured, or nothing was found for the IP.
pub fn geoloc(ip: std::net::IpAddr) -> Option<Geoloc> {
    let mut addr = edge::Address::default();
    match ip {
        std::net::IpAddr::V4(v4) => {
            addr.set_is_v6(false).ok()?;
            addr.set_addr_v4(v4.octets()).ok()?;
        }
        std::net::IpAddr::V6(v6) => {
            addr.set_is_v6(true).ok()?;
            addr.set_addr_v6(v6.octets()).ok()?;
        }
    }
    let req = edge::GeolocRequest { ip: addr, _phantom: std::marker::PhantomData };
    let encoded = req.encode_to_vec().ok()?;
    let mut out = [0u8; 512];
    let written = unsafe {
        geoloc_lookup(encoded.as_ptr(), encoded.len() as u32, out.as_mut_ptr(), out.len() as u32)
    };
    if written == 0 {
        return None;
    }
    let resp = edge::GeolocResponse::decode_exact(&out[..written as usize]).ok()?;
    Some(Geoloc {
        asn: resp.asn,
        asn_org: std::string::String::from_utf8_lossy(&resp.asn_org.data).into_owned(),
        country: std::string::String::from_utf8_lossy(&resp.country.data).into_owned(),
        city: std::string::String::from_utf8_lossy(&resp.city.data).into_owned(),
    })
}


/// This popcache node's serving config as reported by the host (the non-sensitive
/// subset: origin + listener ports). A port of 0 means that listener is not
/// configured.
pub struct PopcacheConfig {
    pub origin: std::string::String,
    pub http_port: u16,
    pub https_port: u16,
    pub http3_port: u16,
}

/// Fetch this popcache node's serving config from the host. None when the host
/// has no config provider configured (or the buffer was too small).
pub fn popcache_config() -> Option<PopcacheConfig> {
    let mut out = [0u8; 2048];
    let written = unsafe { get_popcache_config(out.as_mut_ptr(), out.len() as u32) };
    if written == 0 {
        return None;
    }
    let resp = edge::PopcacheConfig::decode_exact(&out[..written as usize]).ok()?;
    Some(PopcacheConfig {
        origin: std::string::String::from_utf8_lossy(&resp.origin.data).into_owned(),
        http_port: resp.http_port,
        https_port: resp.https_port,
        http3_port: resp.http3_port,
    })
}

/// A fetched HTTP response: status + headers + the full buffered body.
pub struct FetchResponse {
    pub status: u16,
    pub headers: Vec<(std::string::String, std::string::String)>,
    pub body: Vec<u8>,
}

/// Pull a fetch result buffer (info or body) with the grow-retry convention.
fn pull_fetch(id: u32, call: unsafe extern "C" fn(u32, *mut u8, u32) -> u32) -> Vec<u8> {
    let mut buf = vec![0u8; 4096];
    let total = unsafe { call(id, buf.as_mut_ptr(), buf.len() as u32) } as usize;
    if total > buf.len() {
        buf = vec![0u8; total];
        let again = unsafe { call(id, buf.as_mut_ptr(), buf.len() as u32) } as usize;
        buf.truncate(again.min(total));
    } else {
        buf.truncate(total);
    }
    buf
}

/// Issue an outbound HTTP(S) sub-request via the host ("fetch" host call).
///
/// The destination must be covered by the module's registered allowed_hosts
/// (default-deny). `timeout_ms` 0 means the host default; the host clamps it.
/// Err carries either the host's rejection (allowlist/scheme/budget — reported
/// generically, details are in the host log) or the transport error.
pub fn fetch(
    method: &str,
    url: &str,
    headers: &[(&str, &str)],
    body: &[u8],
    timeout_ms: u32,
) -> Result<FetchResponse, std::string::String> {
    let mut req = edge::FetchRequest::default();
    let upper = method.to_ascii_uppercase();
    req.method.method = match upper.as_str() {
        "GET" => edge::Method::GET,
        "POST" => edge::Method::POST,
        "PUT" => edge::Method::PUT,
        "HEAD" => edge::Method::HEAD,
        "OPTIONS" => edge::Method::OPTIONS,
        "PATCH" => edge::Method::PATCH,
        "DELETE" => edge::Method::DELETE,
        "CONNECT" => edge::Method::CONNECT,
        "TRACE" => edge::Method::TRACE,
        _ => edge::Method::OTHER,
    };
    if req.method.method == edge::Method::OTHER {
        req.method
            .set_method_name(std::borrow::Cow::Owned(upper.clone().into_bytes()))
            .map_err(|e| format!("bad method: {:?}", e))?;
    }
    req.url
        .set_data(std::borrow::Cow::Borrowed(url.as_bytes()))
        .map_err(|e| format!("bad url: {:?}", e))?;
    req.header = make_header(headers.iter().copied());
    req.set_body(std::borrow::Cow::Borrowed(body))
        .map_err(|e| format!("bad body: {:?}", e))?;
    req.timeout_ms = timeout_ms;
    let encoded = req
        .encode_to_vec()
        .map_err(|e| format!("encode failed: {:?}", e))?;

    let id = unsafe { host_fetch(encoded.as_ptr(), encoded.len() as u32) };
    if id == 0 {
        return Err("fetch rejected by host (allowlist/scheme/budget)".into());
    }

    let info_buf = pull_fetch(id, get_fetch_response);
    let info = edge::FetchResponseInfo::decode_exact(&info_buf)
        .map_err(|e| format!("bad fetch response info: {:?}", e))?;
    if info.status == 0 {
        let msg = info
            .error()
            .map(|s| std::string::String::from_utf8_lossy(&s.data).into_owned())
            .unwrap_or_else(|| "unknown fetch error".into());
        return Err(msg);
    }
    let mut headers_out = Vec::new();
    if let Some(hdr) = info.header() {
        for f in hdr.fields.iter() {
            headers_out.push((
                std::string::String::from_utf8_lossy(&f.key.data).into_owned(),
                std::string::String::from_utf8_lossy(&f.value.data).into_owned(),
            ));
        }
    }
    Ok(FetchResponse {
        status: info.status,
        headers: headers_out,
        body: pull_fetch(id, get_fetch_body),
    })
}

pub struct ChangeSet<'a> {
    request_changes: edge::ChangeSet<'a>,
    response_changes: edge::ChangeSet<'a>,
}

pub fn make_field<'a>(key: &'a str, value: &'a str) -> edge::Field<'a> {
    let mut field = edge::Field::default();
    field.key.set_data(std::borrow::Cow::Borrowed(key.as_bytes())).unwrap();
    field.value.set_data(std::borrow::Cow::Borrowed(value.as_bytes())).unwrap();
    field
}

pub fn make_header<'a,T : Iterator<Item = (&'a str, &'a str)>>(iter:T) -> edge::Header<'a> {
    let mut header = edge::Header::default();
    header.set_fields(iter.map(|(key,value)| make_field(key,value)).collect()).unwrap(); 
    header
}

pub fn make_body<'a>(data: &'a [u8]) -> edge::Body<'a> {
    let mut body = edge::Body::default();
    body.offset = 0;
    body.set_body(std::borrow::Cow::Borrowed(data)).unwrap();
    body
}

impl<'a> ChangeSet<'a> {
    pub fn new() -> Self {
        ChangeSet { request_changes: edge::ChangeSet::default(), response_changes: edge::ChangeSet::default() }
    }

    pub fn add_request_diff(&mut self, diff: edge::DiffData<'a>) -> &mut Self {
        self.request_changes.len += 1;
        self.request_changes.diff.to_mut().push(diff);
        self
    }

    pub fn add_response_diff(&mut self, diff: edge::DiffData<'a>) -> &mut Self {
        self.response_changes.len += 1;
        self.response_changes.diff.to_mut().push(diff);
        self
    }

    pub fn request_routing(&mut self, routing: edge::Routing) -> &mut Self {
       let mut r = edge::DiffData::default();
       r.diff_type = edge::DiffDataType::routing;
       r.set_routing(routing).unwrap();
       self.add_request_diff(r)
    }

    pub fn response_routing(&mut self, routing: edge::Routing) -> &mut Self {
        let mut r = edge::DiffData::default();
        r.diff_type = edge::DiffDataType::routing;
        r.set_routing(routing).unwrap();
        self.add_response_diff(r)
    }

    pub fn path_info(&mut self, path: edge::PathInfo<'a>) -> &mut Self {
        let mut p = edge::DiffData::default();
        p.diff_type = edge::DiffDataType::path;
        p.set_path(path).unwrap();
        self.add_request_diff(p)
    }

    pub fn path(&mut self, path: &'a str) -> &mut Self {
        let mut pinfo = edge::PathInfo::default();
        pinfo.path.set_data(std::borrow::Cow::Borrowed(path.as_bytes())).unwrap();
        self.path_info(pinfo)
    }

    pub fn method_info(&mut self, method: edge::MethodInfo<'a>) -> &mut Self {
        let mut m = edge::DiffData::default();
        m.diff_type = edge::DiffDataType::method;
        m.set_method(method).unwrap();
        self.add_request_diff(m)
    }

    pub fn method(&mut self, method: edge::Method) -> &mut Self {
        let mut minfo = edge::MethodInfo::default();
        minfo.method = method;
        self.method_info(minfo)
    }

    pub fn request_field_info(&mut self, op: edge::DiffKind, field: edge::Field<'a>) -> &mut Self {
        let mut diff = edge::DiffData::default();
        diff.diff_type = edge::DiffDataType::field;
        diff.kind = op;
        diff.set_field(field).unwrap();
        self.add_request_diff(diff)
    }

    pub fn response_field_info(&mut self, op: edge::DiffKind, field: edge::Field<'a>) -> &mut Self {
        let mut diff = edge::DiffData::default();
        diff.diff_type = edge::DiffDataType::field;
        diff.kind = op;
        diff.set_field(field).unwrap();
        self.add_response_diff(diff)
    }

    pub fn request_field(&mut self, op: edge::DiffKind, key: &'a str, value: &'a str) -> &mut Self {
        let field = make_field(key, value);
        self.request_field_info(op, field)
    }

    pub fn response_field(&mut self, op: edge::DiffKind, key: &'a str, value: &'a str) -> &mut Self {
        let field = make_field(key, value);
        self.response_field_info(op, field)
    }

    pub fn request_header_info(&mut self, op: edge::DiffKind, headers: edge::Header<'a>) -> &mut Self {
        let mut diff = edge::DiffData::default();
        diff.diff_type = edge::DiffDataType::header;
        diff.kind = op;
        diff.set_header(headers).unwrap();
        self.add_request_diff(diff)
    }

    pub fn response_header_info(&mut self, op: edge::DiffKind, headers: edge::Header<'a>) -> &mut Self {
        let mut diff = edge::DiffData::default();
        diff.diff_type = edge::DiffDataType::header;
        diff.kind = op;
        diff.set_header(headers).unwrap();
        self.add_response_diff(diff)
    }

    pub fn request_header<T: Iterator<Item = (&'a str, &'a str)>>(&mut self,op: edge::DiffKind, iter: T) -> &mut Self {
        let headers = make_header(iter);
        self.request_header_info(op, headers)
    }

    pub fn response_header<T: Iterator<Item = (&'a str, &'a str)>>(&mut self,op: edge::DiffKind, iter: T) -> &mut Self {
        let headers = make_header(iter);
        self.response_header_info(op, headers)
    }

    pub fn request_body_info(&mut self,op: edge::DiffKind, body: edge::Body<'a>) -> &mut Self {
        let mut diff = edge::DiffData::default();
        diff.diff_type = edge::DiffDataType::body;
        diff.kind = op;
        diff.set_body(body).unwrap();
        self.add_request_diff(diff)
    }

    pub fn response_body_info(&mut self,op: edge::DiffKind, body: edge::Body<'a>) -> &mut Self {
        let mut diff = edge::DiffData::default();
        diff.diff_type = edge::DiffDataType::body;
        diff.kind = op;
        diff.set_body(body).unwrap();
        self.add_response_diff(diff)
    }

    pub fn request_body(&mut self, data: &'a [u8]) -> &mut Self {
        let body = make_body(data);
        self.request_body_info(edge::DiffKind::replace, body)
    }

    pub fn append_request_body(&mut self, data: &'a [u8]) -> &mut Self {
        let body = make_body(data);
        self.request_body_info(edge::DiffKind::insert, body)
    }

    pub fn response_body(&mut self, data: &'a [u8]) -> &mut Self {
        let body = make_body(data);
        self.response_body_info(edge::DiffKind::replace, body)
    }

    pub fn append_response_body(&mut self, data: &'a [u8]) -> &mut Self {
        let body = make_body(data);
        self.response_body_info(edge::DiffKind::insert, body)
    }

    pub fn status(&mut self, status: u16) -> &mut Self {
        let mut diff = edge::DiffData::default();
        diff.diff_type = edge::DiffDataType::status;
        diff.set_status(status).unwrap();
        self.add_response_diff(diff)
    }

    pub fn apply_request(&self) -> Result<(), Error> {
        if self.request_changes.len == 0 {
            return Ok(()); // No changes to apply
        }
        let encoded = self.request_changes.encode_to_vec()?;
        if encoded.len() > u32::MAX as usize {
            return Err(Error::NotEnoughBuffer);
        }
        let size = encoded.len() as u32;
        let pointer = encoded.as_ptr() as *const u8;
        let result = unsafe {
            change_request_info(pointer, size)
        };
        if result == 0 {
            Err(Error::ApplyChangeFailed(true))
        } else {
            Ok(())
        }
    }

    pub fn apply_response(&self) -> Result<(), Error> {
        if self.response_changes.len == 0 {
            return Ok(()); // No changes to apply
        }
        let encoded = self.response_changes.encode_to_vec()?;
        if encoded.len() > u32::MAX as usize {
            return Err(Error::NotEnoughBuffer);
        }
        let size = encoded.len() as u32;
        let pointer = encoded.as_ptr() as *const u8;
        let result = unsafe {
            change_response_info(pointer, size)
        };
        if result == 0 {
            Err(Error::ApplyChangeFailed(false))
        } else {
            Ok(())
        }
    }

    pub fn apply(&self) -> Result<(), Error> {
        self.apply_request()?;
        self.apply_response()
    }


    // useful methods
    pub fn location(&mut self, location: &'a str) -> &mut Self {
        self.response_field(edge::DiffKind::replace, "Location", location)
    }
}
