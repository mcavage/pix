package positive

import "log"

// PlainLogFatal proves the un-aliased spelling is still caught — a
// regression guard against the alias-resolution rewrite accidentally
// narrowing the check to ONLY the aliased form.
func PlainLogFatal(err error) {
	log.Fatal(err)
}
