package main

// The time-zone database, embedded (TASK-3524, about 450 KB). The Insights
// report buckets in the viewer's IANA zone (`GET /report?tz=`), and
// time.LoadLocation needs a zoneinfo database to resolve one. Linux and macOS
// hosts have one; Windows hosts and scratch containers do not, and there the
// request would answer 400 invalid_tz for every zone. The host's database
// still wins when present; this is the fallback.
import _ "time/tzdata"
