package main
import (
 "crypto/hmac"
 "crypto/sha256"
 "encoding/hex"
 "testing"
 "time"
 "strconv"
)
func TestSignature(t *testing.T){
 secret:="unit-test-secret-value-at-least-32-characters";ts:=strconv.FormatInt(time.Now().UnixMilli(),10)
 body:=[]byte(`{"tenantId":"test"}`)
 mac:=hmac.New(sha256.New,[]byte(secret));mac.Write([]byte(ts+"."));mac.Write(body)
 sig:=hex.EncodeToString(mac.Sum(nil))
 if !authorized(body,ts,sig,secret,time.Now()){t.Fatal("valid signature was denied")}
 if authorized([]byte(`{"tenantId":"other"}`),ts,sig,secret,time.Now()){t.Fatal("forged body was authorized")}
}
func TestManifestDoesNotClaimClioSupport(t *testing.T){
 c,ok:=manifest("clio-manage");if !ok{t.Fatal("missing clio manifest")}
 if len(c.Capabilities)!=0{t.Fatal("unimplemented Clio adapter advertises live capabilities")}
}
