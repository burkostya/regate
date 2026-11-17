package server

import (
    "fmt"
    "net/url"
    "strconv"
    "strings"
    "unicode/utf8"

    "github.com/burkostya/regate/internal/storage"
)

// Console trace queries are exact-match, never regex, SQL or substring
// searches. Parse strictly: duplicate parameters and unknown selectors
// should not yield an unexpectedly broad incident investigation.
// A malformed percent escape must not disappear through URL.Query(), which
// silently drops bad pairs and can broaden an operator's trace inspection.
// Accept only bounded, unambiguous UTF-8 query selectors.
const maxTraceQueryBytes = 1024

func parseTraceRequestQuery(raw string) (storage.TraceQuery,error) {
    if len(raw)>maxTraceQueryBytes {
        return storage.TraceQuery{},fmt.Errorf("trace query exceeds %d bytes",maxTraceQueryBytes)
    }
    values,err:=url.ParseQuery(raw)
    if err!=nil {
        return storage.TraceQuery{},fmt.Errorf("invalid trace query encoding: %w",err)
    }
    return parseTraceQuery(values)
}

func parseTraceQuery(values url.Values) (storage.TraceQuery,error) {
    query:=storage.TraceQuery{Limit:2000}
    for key,items:=range values {
        switch key {
        case "run_id","status","limit":
        default:
            return query,fmt.Errorf("unknown trace filter %q",key)
        }
        if len(items)!=1 {return query,fmt.Errorf("trace filter %q must occur once",key)}
    }
    query.RunID=values.Get("run_id")
    query.Status=values.Get("status")
    if _,present:=values["run_id"];present && query.RunID=="" {return query,fmt.Errorf("trace run_id cannot be empty")}
    if len(query.RunID)>128 || !utf8.ValidString(query.RunID) || strings.ContainsAny(query.RunID,"\r\n\x00") {
        return query,fmt.Errorf("invalid trace run_id filter")
    }
    if _,present:=values["status"];present && query.Status=="" {return query,fmt.Errorf("trace status cannot be empty")}
    if len(query.Status)>48 || !utf8.ValidString(query.Status) || strings.ContainsAny(query.Status,"\r\n\x00") {
        return query,fmt.Errorf("invalid trace status filter")
    }
    if raw,found:=values["limit"];found {
        if raw[0]=="" {return query,fmt.Errorf("trace limit must be 1..2000")}
        limit,err:=strconv.Atoi(raw[0])
        if err!=nil || limit<1 || limit>2000 {
            return query,fmt.Errorf("trace limit must be 1..2000")
        }
        query.Limit=limit
    }
    return query,nil
}
