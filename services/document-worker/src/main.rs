use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::io::{self, Read};

const MAX_INPUT_BYTES: usize = 1_000_000;
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Request { operation: String, bytes_utf8: String }
#[derive(Serialize)]
struct Output { status: &'static str, sha256: String, size_bytes: usize }

fn process(mut reader: impl Read) -> Result<Output, Box<dyn std::error::Error>> {
    // Read one byte past the limit to distinguish exact-size input from truncation.
    let mut input = Vec::new();
    reader.by_ref().take((MAX_INPUT_BYTES + 1) as u64).read_to_end(&mut input)?;
    if input.len() > MAX_INPUT_BYTES { return Err("INPUT_TOO_LARGE".into()); }
    let req: Request = serde_json::from_slice(&input)?;
    if req.operation != "fingerprint.v1" { return Err("UNSUPPORTED_CAPABILITY".into()); }
    let bytes = req.bytes_utf8.as_bytes();
    Ok(Output { status: "ok", sha256: hex::encode(Sha256::digest(bytes)), size_bytes: bytes.len() })
}
fn main() -> Result<(), Box<dyn std::error::Error>> {
    // Synthetic UTF-8 fingerprinting only; this is not a PDF parser.
    let out = process(io::stdin().lock())?;
    println!("{}", serde_json::to_string(&out)?);
    Ok(())
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn known_fingerprint_and_utf8_byte_count() {
        let out=process(r#"{"operation":"fingerprint.v1","bytes_utf8":"abc"}"#.as_bytes()).unwrap();
        assert_eq!(out.sha256,"ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");
        assert_eq!(out.size_bytes,3);
        assert_eq!(process(r#"{"operation":"fingerprint.v1","bytes_utf8":"é"}"#.as_bytes()).unwrap().size_bytes,2);
    }
    #[test]
    fn refuses_truncated_unknown_and_oversized_input() {
        assert!(process(&vec![b' ';MAX_INPUT_BYTES+1][..]).is_err());
        assert!(process(r#"{"operation":"fingerprint.v1","bytes_utf8":"abc","extra":true}"#.as_bytes()).is_err());
        assert!(process(r#"{"operation":"other","bytes_utf8":"abc"}"#.as_bytes()).is_err());
        assert!(process(b"{bad".as_slice()).is_err());
    }
}
