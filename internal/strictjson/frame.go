// Package strictjson validates bounded external JSON frames without
// silently accepting duplicate members or trailing data.
package strictjson

import (
    "bytes"
    "encoding/json"
    "fmt"
    "io"
    "unicode/utf8"
)

const MaxDepth = 128

// Check validates a complete JSON value, rejecting duplicate object names
// after escape normalization, invalid UTF-8 and excessive nesting. It does
// not require the top-level value to be an object (the MCP SDK owns that
// request-shape check).
func Check(raw []byte) error {
    if !utf8.Valid(raw) { return fmt.Errorf("invalid UTF-8 in JSON") }
    decoder:=json.NewDecoder(bytes.NewReader(raw))
    decoder.UseNumber()
    var walk func(int)error
    walk=func(depth int)error {
        if depth>MaxDepth {return fmt.Errorf("JSON nesting exceeds %d levels",MaxDepth)}
        token,err:=decoder.Token()
        if err!=nil {return fmt.Errorf("invalid JSON: %w",err)}
        delim,ok:=token.(json.Delim)
        if !ok {return nil}
        switch delim {
        case '{':
            names:=map[string]struct{}{}
            for decoder.More() {
                token,err:=decoder.Token()
                if err!=nil {return fmt.Errorf("invalid JSON member: %w",err)}
                key,ok:=token.(string)
                if !ok {return fmt.Errorf("invalid JSON member name")}
                if _,exists:=names[key];exists {
                    // Do not echo an untrusted key: it could contain a secret.
                    return fmt.Errorf("duplicate JSON object member")
                }
                names[key]=struct{}{}
                if err:=walk(depth+1);err!=nil {return err}
            }
            end,err:=decoder.Token()
            if err!=nil || end!=json.Delim('}') {
                return fmt.Errorf("invalid JSON object closing delimiter")
            }
        case '[':
            for decoder.More() {
                if err:=walk(depth+1);err!=nil {return err}
            }
            end,err:=decoder.Token()
            if err!=nil || end!=json.Delim(']') {
                return fmt.Errorf("invalid JSON array closing delimiter")
            }
        default:
            return fmt.Errorf("unexpected JSON delimiter")
        }
        return nil
    }
    if err:=walk(0);err!=nil {return err}
    if _,err:=decoder.Token();err!=io.EOF {
        if err==nil {return fmt.Errorf("extra JSON value after first frame")}
        return fmt.Errorf("trailing invalid JSON: %w",err)
    }
    return nil
}

// ReadObject bounds the ENTIRE response (not just its first JSON value).
// A truncated but otherwise valid JSON prefix must never be accepted.
func ReadObject(reader io.Reader, maxBytes int64) (map[string]any,error) {
    if maxBytes<1 {return nil,fmt.Errorf("invalid JSON size limit")}
    raw,err:=io.ReadAll(io.LimitReader(reader,maxBytes+1))
    if err!=nil {return nil,fmt.Errorf("read JSON body: %w",err)}
    if int64(len(raw))>maxBytes {
        return nil,fmt.Errorf("JSON body exceeds %d bytes",maxBytes)
    }
    if err:=Check(raw);err!=nil {return nil,err}
    var object map[string]any
    if err:=json.Unmarshal(raw,&object);err!=nil {
        return nil,fmt.Errorf("decode JSON object: %w",err)
    }
    if object==nil {return nil,fmt.Errorf("expected JSON object")}
    return object,nil
}
