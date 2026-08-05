// doctorfixture_test.go — copies of doctor's test-only fixtures that cmd/pix
// tests still need. Copies, not exports: scaffolding has no business in a
// package's public API, and a bulk export pass will happily put it there.
package main

import (
	"time"
)

var receiptClock = func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }

// TestMCPAttachmentFromReceipt covers the attachment axis: ONLY the launcher

// receipt (a record of successful pix actions) may claim attachment.
