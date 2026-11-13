"""Synthetic PDF/OCR runtime smoke test in the actual isolated parser image."""
import base64,hashlib,json,re,subprocess,sys
stream=b'BT /F1 20 Tf 40 100 Td (LEASE AGREEMENT) Tj ET'
objects=[b'<< /Type /Catalog /Pages 2 0 R >>',b'<< /Type /Pages /Kids [3 0 R] /Count 1 >>',b'<< /Type /Page /Parent 2 0 R /MediaBox [0 0 400 200] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>',b'<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>',b'<< /Length '+str(len(stream)).encode()+b' >>\nstream\n'+stream+b'\nendstream']
raw=bytearray(b'%PDF-1.4\n');offsets=[]
for i,obj in enumerate(objects,1):
    offsets.append(len(raw));raw.extend(str(i).encode()+b' 0 obj\n'+obj+b'\nendobj\n')
xref=len(raw);raw.extend(b'xref\n0 6\n0000000000 65535 f \n')
for offset in offsets:raw.extend(f'{offset:010} 00000 n \n'.encode())
raw.extend(f'trailer\n<< /Size 6 /Root 1 0 R >>\nstartxref\n{xref}\n%%EOF\n'.encode())
request={'operation':'parse.v1','media_type':'application/pdf','bytes_base64':base64.b64encode(raw).decode()}
result=subprocess.run(['docker','run','--rm','-i','--network','none','--read-only','--cap-drop','ALL','--security-opt','no-new-privileges:true','--pids-limit','32','--memory','384m','--tmpfs','/tmp:rw,noexec,nosuid,size=64m','--env','DOCUMENT_OCR_MODE=all-pages','--entrypoint','/usr/local/bin/lawpilot-document-worker',sys.argv[1]],input=json.dumps(request).encode(),capture_output=True,timeout=60)
assert result.returncode==0,'isolated parser runtime rejected synthetic fixture'
output=json.loads(result.stdout);assert output['sha256']==hashlib.sha256(raw).hexdigest();assert output['parser_version']=='isolated-ocr.v1';assert output['pages'][0]['number']==1
assert 'leaseagreement' in re.sub('[^a-z]','',output['pages'][0]['text'].lower())
print('PASS: isolated non-root image rasterizes and OCRs a synthetic PDF without network or writable root')
