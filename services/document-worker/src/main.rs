use base64::{engine::general_purpose::STANDARD, Engine};
use quick_xml::{events::Event, Reader};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::{
    collections::HashSet,
    io::{self, Cursor, Read, Write},
    process::{Command, Stdio},
};
const MAX_FILE: usize = 10 * 1024 * 1024;
const MAX_TEXT: usize = 262144;
const MAX_PAGES: usize = 200;
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Request {
    operation: String,
    #[serde(default)]
    bytes_utf8: String,
    #[serde(default)]
    bytes_base64: String,
    #[serde(default)]
    media_type: String,
}
#[derive(Serialize)]
struct Page {
    number: usize,
    text: String,
}
#[derive(Serialize)]
struct Output {
    status: &'static str,
    sha256: String,
    size_bytes: usize,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pages: Vec<Page>,
    #[serde(skip_serializing_if = "Option::is_none")]
    parser_version: Option<&'static str>,
}
type Result<T> = std::result::Result<T, Box<dyn std::error::Error>>;
fn pdf(bytes: &[u8]) -> Result<Vec<Page>> {
    if !bytes.starts_with(b"%PDF-") {
        return Err("PDF_SIGNATURE_REQUIRED".into());
    }
    // Constant argv, stdin input and no shell or caller-selected filesystem paths.
    // The parent gRPC worker enforces the wall deadline and kills the process tree.
    let mut child = Command::new("pdftotext")
        .args(["-layout", "-enc", "UTF-8", "-f", "1", "-l", "201", "-", "-"])
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .spawn()?;
    let mut input = child.stdin.take().ok_or("PARSER_PIPE")?;
    let owned = bytes.to_vec();
    let writer = std::thread::spawn(move || {
        let _ = input.write_all(&owned);
    });
    let mut data = Vec::new();
    child
        .stdout
        .take()
        .ok_or("PARSER_PIPE")?
        .take((MAX_TEXT + MAX_PAGES * 4 + 1) as u64)
        .read_to_end(&mut data)?;
    if data.len() > MAX_TEXT + MAX_PAGES * 4 {
        let _ = child.kill();
        let _ = child.wait();
        return Err("EXTRACTED_TEXT_TOO_LARGE".into());
    }
    let ok = child.wait()?.success();
    let _ = writer.join();
    if !ok {
        return Err("PDF_PARSE_FAILED_OR_ENCRYPTED".into());
    }
    let text = String::from_utf8(data)?;
    let mut parts: Vec<&str> = text.split('\u{c}').collect();
    if parts.last() == Some(&"") {
        parts.pop();
    }
    if parts.len() > MAX_PAGES {
        return Err("PAGE_LIMIT".into());
    }
    let mode = std::env::var("DOCUMENT_OCR_MODE").unwrap_or_else(|_| "off".into());
    if mode != "off" && mode != "empty-pages" && mode != "all-pages" {
        return Err("INVALID_OCR_MODE".into());
    }
    let mut pages = Vec::new();
    let mut ocr_count = 0;
    let maximum = std::env::var("DOCUMENT_OCR_MAX_PAGES")
        .unwrap_or_else(|_| "10".into())
        .parse::<usize>()?;
    if maximum == 0 || maximum > 20 {
        return Err("OCR_PAGE_LIMIT".into());
    }
    for (i, part) in parts.into_iter().enumerate() {
        let mut text = part.to_owned();
        if mode == "all-pages" || mode == "empty-pages" && text.trim().is_empty() {
            ocr_count += 1;
            if ocr_count > maximum {
                return Err("OCR_PAGE_LIMIT".into());
            }
            text = ocr_page(bytes, i + 1)?;
        }
        pages.push(Page {
            number: i + 1,
            text,
        });
    }
    Ok(pages)
}
fn ocr_page(bytes: &[u8], page: usize) -> Result<String> {
    let dir = tempfile::tempdir()?;
    let input = dir.path().join("source.pdf");
    let prefix = dir.path().join("page");
    std::fs::write(&input, bytes)?;
    let status = Command::new("pdftoppm")
        .args([
            "-f",
            &page.to_string(),
            "-l",
            &page.to_string(),
            "-singlefile",
            "-r",
            "180",
            "-scale-to",
            "2200",
            "-gray",
            "-png",
        ])
        .arg(&input)
        .arg(&prefix)
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status()?;
    let image = dir.path().join("page.png");
    if !status.success() || std::fs::metadata(&image)?.len() > 16 * 1024 * 1024 {
        return Err("OCR_RASTER_LIMIT".into());
    }
    let lang = std::env::var("DOCUMENT_OCR_LANGUAGES").unwrap_or_else(|_| "eng".into());
    if lang.len() > 80
        || !lang
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || c == '_' || c == '+')
    {
        return Err("OCR_LANGUAGE_CONFIGURATION".into());
    }
    let mut child = Command::new("tesseract")
        .arg(&image)
        .args(["stdout", "-l", &lang, "--psm", "3"])
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .spawn()?;
    let mut output = Vec::new();
    child
        .stdout
        .take()
        .ok_or("OCR_PIPE")?
        .take((MAX_TEXT + 1) as u64)
        .read_to_end(&mut output)?;
    if output.len() > MAX_TEXT {
        let _ = child.kill();
        let _ = child.wait();
        return Err("OCR_TEXT_LIMIT".into());
    }
    if !child.wait()?.success() {
        return Err("OCR_UNAVAILABLE".into());
    }
    Ok(String::from_utf8(output)?)
}

fn docx(bytes: &[u8]) -> Result<Vec<Page>> {
    if !bytes.starts_with(b"PK\x03\x04") {
        return Err("DOCX_SIGNATURE_REQUIRED".into());
    }
    let mut archive = zip::ZipArchive::new(Cursor::new(bytes))?;
    if archive.len() > 512 {
        return Err("ARCHIVE_ENTRY_LIMIT".into());
    }
    let mut seen = HashSet::new();
    let mut total = 0u64;
    let mut documents = Vec::new();
    let mut content_type = false;
    let mut has_document = false;
    for i in 0..archive.len() {
        let mut file = archive.by_index(i)?;
        let name = file.name().to_owned();
        if !seen.insert(name.clone())
            || file.enclosed_name().is_none()
            || name.contains('\\')
            || name.split('/').any(|s| s == "..")
            || name.contains("vbaProject")
            || name.contains("/embeddings/")
            || name.contains("/activeX/")
            || file.size() > 8 * 1024 * 1024
            || file.size() > file.compressed_size().max(1) * 1000
        {
            return Err("UNSAFE_DOCX_ARCHIVE".into());
        }
        total = total.checked_add(file.size()).ok_or("ARCHIVE_LIMIT")?;
        if total > 32 * 1024 * 1024 {
            return Err("ARCHIVE_LIMIT".into());
        }
        let text_part = name == "word/document.xml"
            || name == "word/footnotes.xml"
            || name == "word/endnotes.xml"
            || (name.starts_with("word/header") || name.starts_with("word/footer"))
                && name.ends_with(".xml");
        if text_part || name == "[Content_Types].xml" || name.ends_with(".rels") {
            let mut data = Vec::new();
            file.by_ref()
                .take(8 * 1024 * 1024 + 1)
                .read_to_end(&mut data)?;
            if data.len() > 8 * 1024 * 1024 {
                return Err("XML_LIMIT".into());
            }
            let xml = std::str::from_utf8(&data)?;
            if xml.contains("<!DOCTYPE") || xml.contains("<!ENTITY") {
                return Err("XML_ENTITY_FORBIDDEN".into());
            }
            if name.ends_with(".rels") {
                let mut reader = Reader::from_str(xml);
                loop {
                    match reader.read_event()? {
                        Event::Start(e) | Event::Empty(e) => {
                            for a in e.attributes() {
                                let a = a?;
                                if a.key.as_ref() == b"TargetMode"
                                    && a.value.as_ref() == b"External"
                                {
                                    return Err("EXTERNAL_DOCX_RELATIONSHIP".into());
                                }
                            }
                        }
                        Event::DocType(_) => return Err("XML_ENTITY_FORBIDDEN".into()),
                        Event::Eof => break,
                        _ => {}
                    }
                }
            }
            if name == "[Content_Types].xml" {
                content_type = xml.contains("wordprocessingml.document.main+xml")
                    && !xml.contains("macroEnabled");
            }
            if text_part {
                if name == "word/document.xml" {
                    has_document = true;
                }
                documents.push((name, xml.to_owned()));
            }
        }
    }
    if !content_type || !has_document {
        return Err("DOCX_DOCUMENT_REQUIRED".into());
    }
    documents.sort_by_key(|(name, _)| (name != "word/document.xml", name.clone()));
    let mut text = String::new();
    for (name, xml) in documents {
        if name != "word/document.xml" {
            text.push_str(&format!("\n[Document part: {}]\n", name));
        }
        let mut reader = Reader::from_str(&xml);
        let mut in_text = false;
        loop {
            match reader.read_event()? {
                Event::Start(e) => match e.local_name().as_ref() {
                    b"t" => in_text = true,
                    b"ins" | b"del" | b"moveFrom" | b"moveTo" | b"instrText" | b"fldChar"
                    | b"altChunk" => return Err("UNRESOLVED_DOCX_CONTENT".into()),
                    _ => {}
                },
                Event::End(e) => {
                    if e.local_name().as_ref() == b"t" {
                        in_text = false;
                    }
                    if e.local_name().as_ref() == b"p" {
                        text.push('\n');
                    }
                }
                Event::Empty(e) => match e.local_name().as_ref() {
                    b"tab" => text.push('\t'),
                    b"br" => text.push('\n'),
                    b"fldChar" | b"fldSimple" | b"altChunk" => {
                        return Err("UNRESOLVED_DOCX_CONTENT".into())
                    }
                    _ => {}
                },
                Event::Text(e) if in_text => {
                    text.push_str(&quick_xml::escape::unescape(&e.decode()?)?);
                }
                Event::GeneralRef(e) if in_text => {
                    let name = e.decode()?;
                    text.push_str(&quick_xml::escape::unescape(&format!("&{};", name))?);
                }
                Event::CData(e) if in_text => text.push_str(&e.decode()?),
                Event::DocType(_) => return Err("XML_ENTITY_FORBIDDEN".into()),
                Event::Eof => break,
                _ => {}
            }
            if text.len() > MAX_TEXT {
                return Err("EXTRACTED_TEXT_TOO_LARGE".into());
            }
        }
    }
    // DOCX is a flowing document. Do not invent rendered page numbers.
    Ok(vec![Page { number: 0, text }])
}
fn process(mut reader: impl Read) -> Result<Output> {
    let mut input = Vec::new();
    reader
        .by_ref()
        .take((MAX_FILE * 2 + 1) as u64)
        .read_to_end(&mut input)?;
    if input.len() > MAX_FILE * 2 {
        return Err("INPUT_TOO_LARGE".into());
    }
    let req: Request = serde_json::from_slice(&input)?;
    if req.operation == "fingerprint.v1" {
        if input.len() > 1_000_000 || !req.bytes_base64.is_empty() || !req.media_type.is_empty() {
            return Err("INVALID_FINGERPRINT".into());
        }
        return Ok(Output {
            status: "ok",
            sha256: hex::encode(Sha256::digest(req.bytes_utf8.as_bytes())),
            size_bytes: req.bytes_utf8.len(),
            pages: vec![],
            parser_version: None,
        });
    }
    if req.operation != "parse.v1" || !req.bytes_utf8.is_empty() {
        return Err("UNSUPPORTED_CAPABILITY".into());
    }
    let bytes = STANDARD.decode(req.bytes_base64)?;
    if bytes.is_empty() || bytes.len() > MAX_FILE {
        return Err("FILE_LIMIT".into());
    }
    let pages = match req.media_type.as_str() {
        "application/pdf" => pdf(&bytes)?,
        "application/vnd.openxmlformats-officedocument.wordprocessingml.document" => docx(&bytes)?,
        _ => return Err("UNSUPPORTED_MEDIA_TYPE".into()),
    };
    if pages.iter().map(|p| p.text.len()).sum::<usize>() > MAX_TEXT
        || pages.iter().all(|p| p.text.trim().is_empty())
        || pages.iter().any(|p| p.text.contains('\0'))
    {
        return Err("NO_EXTRACTABLE_TEXT_OR_LIMIT".into());
    }
    Ok(Output {
        status: "ok",
        sha256: hex::encode(Sha256::digest(&bytes)),
        size_bytes: bytes.len(),
        pages,
        parser_version: Some(
            if req.media_type == "application/pdf"
                && std::env::var("DOCUMENT_OCR_MODE").unwrap_or_else(|_| "off".into()) != "off"
            {
                "isolated-ocr.v1"
            } else {
                "isolated-text.v1"
            },
        ),
    })
}
fn main() {
    match process(io::stdin().lock()) {
        Ok(value) => println!(
            "{}",
            serde_json::to_string(&value).expect("output serialization")
        ),
        Err(_) => {
            eprintln!("DOCUMENT_PROCESSING_REJECTED");
            std::process::exit(2);
        }
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn fingerprint_compatible() {
        let out =
            process(br#"{"operation":"fingerprint.v1","bytes_utf8":"abc"}"#.as_slice()).unwrap();
        assert_eq!(
            out.sha256,
            "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
        );
        assert_eq!(out.size_bytes, 3);
    }
    #[test]
    fn invalid_and_oversized() {
        assert!(process(&vec![b' '; MAX_FILE * 2 + 1][..]).is_err());
        assert!(process(
            br#"{"operation":"parse.v1","bytes_base64":"AA==","media_type":"application/pdf"}"#
                .as_slice()
        )
        .is_err());
        assert!(process(br#"{"operation":"x","bytes_utf8":"a","extra":true}"#.as_slice()).is_err());
    }
    #[test]
    fn docx_text_and_external_rejection() {
        fn file(external: bool) -> Vec<u8> {
            let mut w = zip::ZipWriter::new(Cursor::new(Vec::new()));
            let o = zip::write::SimpleFileOptions::default();
            w.start_file("[Content_Types].xml", o).unwrap();
            w.write_all(b"<Types>wordprocessingml.document.main+xml</Types>")
                .unwrap();
            w.start_file("word/document.xml", o).unwrap();
            w.write_all(b"<w:document xmlns:w='x'><w:p><w:r><w:t>Lease &amp; terms</w:t></w:r></w:p></w:document>").unwrap();
            if external {
                w.start_file("word/_rels/document.xml.rels", o).unwrap();
                w.write_all(b"<Relationships><Relationship TargetMode='External' Target='https://attacker.invalid'/></Relationships>").unwrap();
            }
            w.finish().unwrap().into_inner()
        };
        let p = docx(&file(false)).unwrap();
        assert_eq!(p[0].number, 0);
        assert!(p[0].text.contains("Lease & terms"));
        assert!(docx(&file(true)).is_err());
    }
}

#[cfg(test)]
mod coverage_tests {
    use super::*;
    #[test]
    fn pdf_preserves_actual_page_numbers() {
        let stream = "BT /F1 12 Tf 40 100 Td (Lease agreement. Rent due monthly.) Tj ET";
        let objects=vec!["<< /Type /Catalog /Pages 2 0 R >>".to_string(),"<< /Type /Pages /Kids [3 0 R] /Count 1 >>".to_string(),"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 200] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>".to_string(),"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>".to_string(),format!("<< /Length {} >>\nstream\n{}\nendstream",stream.len(),stream)];
        let mut raw = String::from("%PDF-1.4\n");
        let mut offsets = vec![0];
        for (i, obj) in objects.iter().enumerate() {
            offsets.push(raw.len());
            raw.push_str(&format!("{} 0 obj\n{}\nendobj\n", i + 1, obj));
        }
        let xref = raw.len();
        raw.push_str("xref\n0 6\n0000000000 65535 f \n");
        for offset in &offsets[1..] {
            raw.push_str(&format!("{:010} 00000 n \n", offset));
        }
        raw.push_str(&format!(
            "trailer\n<< /Size 6 /Root 1 0 R >>\nstartxref\n{}\n%%EOF\n",
            xref
        ));
        let pages = pdf(raw.as_bytes()).unwrap();
        assert_eq!(pages.len(), 1);
        assert_eq!(pages[0].number, 1);
        assert!(pages[0].text.contains("Rent due monthly."));
        let recognized = ocr_page(raw.as_bytes(), 1).unwrap();
        let normalized: String = recognized
            .chars()
            .filter(|c| c.is_ascii_alphanumeric())
            .flat_map(|c| c.to_lowercase())
            .collect();
        assert!(
            normalized.contains("leaseagreement"),
            "OCR fixture text: {:?}",
            recognized
        );
    }
    #[test]
    fn docx_includes_footnotes_and_rejects_tracked_changes() {
        fn build(tracked: bool) -> Vec<u8> {
            let mut w = zip::ZipWriter::new(Cursor::new(Vec::new()));
            let o = zip::write::SimpleFileOptions::default();
            for(name,text)in [("[Content_Types].xml","<Types>wordprocessingml.document.main+xml</Types>"),("word/document.xml",if tracked{"<w:document xmlns:w='x'><w:ins><w:t>unapproved revision</w:t></w:ins></w:document>"}else{"<w:document xmlns:w='x'><w:p><w:r><w:t>Lease</w:t></w:r></w:p></w:document>"}),("word/footnotes.xml","<w:footnotes xmlns:w='x'><w:p><w:r><w:t>Critical qualification</w:t></w:r></w:p></w:footnotes>")]{w.start_file(name,o).unwrap();w.write_all(text.as_bytes()).unwrap();}
            w.finish().unwrap().into_inner()
        }
        assert!(docx(&build(false)).unwrap()[0]
            .text
            .contains("Critical qualification"));
        assert!(docx(&build(true)).is_err());
    }
}
