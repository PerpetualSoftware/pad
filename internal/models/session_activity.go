package models

import "time"

// SessionActivityReportBound is the longest a detached pad session activity
// report may live. The server's reorder window is derived from this bound.
const SessionActivityReportBound = 3 * time.Second
