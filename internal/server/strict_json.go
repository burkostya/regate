package server

import (
    "bytes"
    "encoding/json"
    "fmt"
    "io"
)

// Admin operations may activate revisions, write external effects, change
// capabilities and decrypt grant-related state. Never accept a valid JSON
// prefix followed by unexamined bytes, duplicate keys, or an oversized
// payload. Even a duplicate nested key can be interpreted differently by
// distinct components along the administrative path.
const maxAdminJSONBytes int64 = 2 << 20
const maxAdminJSONDepth = 128

func strictJSONObject(reader io.Reader) (map[string]any,error) {
    raw,err:=io.ReadAll(io.LimitReader(reader,maxAdminJSONBytes+1))
    if err!=nil {return nil,fmt.Errorf("read admin JSON: %w",err)}
    if int64(len(raw))>maxAdminJSONBytes {
        return nil,fmt.Errorf("admin JSON body exceeds %d bytes",maxAdminJSONBytes)
    }
    if err=checkUniqueJSONKeys(raw);err!=nil {return nil,err}
    dec:=json.NewDecoder(bytes.NewReader(raw))
    var object map[string]any
    if err=dec.Decode(&object);err!=nil {
        return nil,fmt.Errorf("decode admin JSON object: %w",err)
    }
    if object==nil {return nil,fmt.Errorf("expected JSON object")}
    var trailing any
    if err=dec.Decode(&trailing);err!=io.EOF {
        if err==nil {return nil,fmt.Errorf("extra JSON value after admin request")}
        return nil,fmt.Errorf("trailing admin JSON data: %w",err)
    }
    return object,nil
}

// Token-based structural validation catches ambiguous duplicate member
// names at any nesting depth. Decoder.Token honors JSON string escaping,
// unlike regex or substring searches; the byte and depth caps bound work.
func checkUniqueJSONKeys(raw []byte) error {
    decoder:=json.NewDecoder(bytes.NewReader(raw))
    var walk func(int)error
    walk=func(depth int)error {
        if depth>maxAdminJSONDepth {
            return fmt.Errorf("admin JSON exceeds nesting depth %d",maxAdminJSONDepth)
        }
        token,err:=decoder.Token()
        if err!=nil {return fmt.Errorf("invalid admin JSON: %w",err)}
        delimiter,ok:=token.(json.Delim)
        if !ok {return nil}
        switch delimiter {
        case '{':
            seen:=map[string]struct{}{}
            for decoder.More() {
                token,err=decoder.Token()
                if err!=nil {return fmt.Errorf("invalid admin object key: %w",err)}
                key,ok:=token.(string)
                if !ok {return fmt.Errorf("invalid admin object member")}
                if _,exists:=seen[key];exists {
                    return fmt.Errorf("duplicate admin JSON key %q",key)
                }
                seen[key]=struct{}{}
                if err:=walk(depth+1);err!=nil {return err}
            }
            closing,err:=decoder.Token()
            if err!=nil {return fmt.Errorf("invalid admin JSON object: %w",err)}
            if closing!=json.Delim('}') {return fmt.Errorf("invalid admin JSON object closing")}
        case '[':
            for decoder.More() {
                if err:=walk(depth+1);err!=nil {return err}
            }
            closing,err:=decoder.Token()
            if err!=nil {return fmt.Errorf("invalid admin JSON array: %w",err)}
            if closing!=json.Delim(']') {return fmt.Errorf("invalid admin JSON array closing")}
        default:
            return fmt.Errorf("unexpected JSON delimiter %q",delimiter)
        }
        return nil
    }
    if err:=walk(0);err!=nil {return err}
    if _,err:=decoder.Token();err!=io.EOF {
        if err==nil {return fmt.Errorf("extra JSON value after admin request")}
        return fmt.Errorf("trailing admin JSON data: %w",err)
    }
    return nil
}
