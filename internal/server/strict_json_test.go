package server

import (
    "bytes"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "strconv"
    "strings"
    "testing"
)

func TestStrictAdminJSONRejectsAmbiguousAndOversizedBodiesBeforeMutation(t *testing.T) {
    svc,server,_:=fixture(t)
    draft,err:=svc.Store.DraftCreate()
    if err!=nil {t.Fatal(err)}
    source:=draft.Source+"\n# admin-body-mutation-marker\n"
    body:=fmt.Sprintf(`{"draft_id":%s,"source":%s}`,strconv.Quote(draft.ID),strconv.Quote(source))
    rawPost:=func(raw string) int {
        t.Helper()
        req,err:=http.NewRequest("POST",server.URL+"/api/draft-update",strings.NewReader(raw))
        if err!=nil {t.Fatal(err)}
        req.Header.Set("Authorization","Bearer test-secret")
        req.Header.Set("Content-Type","application/json")
        response,err:=http.DefaultClient.Do(req)
        if err!=nil {t.Fatal(err)}
        defer response.Body.Close()
        io.Copy(io.Discard,response.Body)
        return response.StatusCode
    }
    malformed:=[]struct{name,body string}{
        {"second-object",body+"{}"},
        {"trailing-junk",body+"\nnot-json"},
        {"oversized-valid-prefix",body+strings.Repeat(" ",int(maxAdminJSONBytes))},
        {"duplicate-root-key",fmt.Sprintf(`{"draft_id":%s,"source":%s,"source":%s}`,strconv.Quote(draft.ID),strconv.Quote(source),strconv.Quote("evil"))},
        {"duplicate-escaped-key",fmt.Sprintf(`{"draft_id":%s,"source":%s,"sour\u0063e":%s}`,strconv.Quote(draft.ID),strconv.Quote(source),strconv.Quote("evil"))},
        {"duplicate-nested-key",fmt.Sprintf(`{"draft_id":%s,"source":%s,"metadata":{"policy":"allow","policy":"deny"}}`,strconv.Quote(draft.ID),strconv.Quote(source))},
        {"null","null"},
        {"array","[]"},
        {"unexpected-type","42"},
        {"deep-array",strings.Repeat("[",maxAdminJSONDepth+1)+"0"+strings.Repeat("]",maxAdminJSONDepth+1)},
    }
    for _,tc:=range malformed {
        t.Run(tc.name,func(t *testing.T){
            if status:=rawPost(tc.body);status!=400 {
                t.Fatalf("invalid JSON was admitted: %s returned %d",tc.name,status)
            }
            current,err:=svc.Store.DraftGet(draft.ID)
            if err!=nil {t.Fatal(err)}
            if current.Source!=draft.Source || current.SourceDigest!=draft.SourceDigest {
                t.Fatalf("malformed admin request mutated draft: %#v",current)
            }
        })
    }
    if status:=rawPost(body);status!=200 {
        t.Fatalf("valid single admin JSON object rejected: HTTP %d",status)
    }
    updated,err:=svc.Store.DraftGet(draft.ID)
    if err!=nil || updated.Source!=source {
        t.Fatalf("valid admin operation was not committed: %#v %v",updated,err)
    }
}

func TestStrictAdminJSONExactBodySizeAndNestedDuplicateKeys(t *testing.T) {
    prefix:=`{"padding":"`
    suffix:=`"}`
    payload:=prefix+strings.Repeat("a",int(maxAdminJSONBytes)-len(prefix)-len(suffix))+suffix
    parsed,err:=strictJSONObject(strings.NewReader(payload))
    if err!=nil || len(parsed["padding"].(string))!=int(maxAdminJSONBytes)-len(prefix)-len(suffix) {
        t.Fatalf("exact body-size limit unexpectedly rejected: %v",err)
    }
    if _,err:=strictJSONObject(strings.NewReader(payload+" "));err==nil {
        t.Fatal("limit+1 payload accepted")
    }
    // JSON escape-normalized duplicate keys must also be rejected by the
    // token-based validator, including duplicates nested inside arrays.
    for _,raw:=range []string{
        `{"a":[{"name":"a","na\u006de":"b"}]}`,
        `{"a":{"scope":"one","scope":"two"}}`,
        `{"a":"hello","a":"world"}`,
    } {
        if _,err:=strictJSONObject(strings.NewReader(raw));err==nil ||
            !strings.Contains(err.Error(),"duplicate") {
            t.Fatalf("duplicate keys not detected in %s: %v",raw,err)
        }
    }
    for _,raw:=range []string{
        `{"a":[{"name":"a"},{"name":"b"}]}`,
        `{"a":{"scope":"one","other":"two"}}`,
        "{\"a\":\"hello\"}   \n",
    }{
        if _,err:=strictJSONObject(strings.NewReader(raw));err!=nil {
            t.Fatalf("valid nested structure rejected %s: %v",raw,err)
        }
    }
}

func FuzzStrictAdminJSONObjectFraming(f *testing.F) {
    for _,body:=range []string{
        `{}`,`{"a":true}`,`{"a":{"b":[1,2,{"c":null}]}}`,
        `{"a":1,"a":2}`,`{}{}`,`{}junk`,`null`,`[]`,
        `{"a":[{"x":"a","x":"b"}]}`,
    } {f.Add([]byte(body))}
    f.Fuzz(func(t *testing.T,body []byte){
        object,err:=strictJSONObject(bytes.NewReader(body))
        if err!=nil {return}
        if object==nil || !json.Valid(body) {
            t.Fatalf("non-object or invalid concatenated JSON admitted: %q",body)
        }
        if err:=checkUniqueJSONKeys(body);err!=nil {
            t.Fatalf("ambiguous JSON member names admitted: %q %v",body,err)
        }
    })
}
