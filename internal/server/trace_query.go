package server

import (
    "fmt"
    "net/url"
    "strconv"
    "strings"

    "github.com/burkostya/regate/internal/storage"
)

// Console trace queries are exact-match, never regex, SQL or substring
// searches. Parse strictly: duplicate parameters and unknown selectors
// should not yield an unexpectedly broad incident investigation.
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
    if len(query.RunID)>128 || strings.ContainsAny(query.RunID,"\r\n\x00") {
        return query,fmt.Errorf("invalid trace run_id filter")
    }
    if len(query.Status)>48 || strings.ContainsAny(query.Status,"\r\n\x00") {
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
