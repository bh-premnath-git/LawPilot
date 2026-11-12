use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::io::{self, Read};

#[derive(Deserialize)]
struct Request { operation: String, bytes_utf8: String }
#[derive(Serialize)]
struct Output { status: &'static str, sha256: String, size_bytes: usize }
fn main() -> Result<(), Box<dyn std::error::Error>> {
    // Sample only: input is synthetic UTF-8 data, not an untrusted PDF parser.
    let mut input = String::new();
    io::stdin().take(1_000_000).read_to_string(&mut input)?;
    let req: Request = serde_json::from_str(&input)?;
    if req.operation != "fingerprint.v1" { return Err("UNSUPPORTED_CAPABILITY".into()); }
    let bytes = req.bytes_utf8.as_bytes();
    let out = Output { status: "ok", sha256: hex::encode(Sha256::digest(bytes)), size_bytes: bytes.len() };
    println!("{}", serde_json::to_string(&out)?);
    Ok(())
}
